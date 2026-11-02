package types

import (
	"encoding/json"
	"regexp"
	"strconv"

	errorsmod "cosmossdk.io/errors"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	proto "github.com/cosmos/gogoproto/proto"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// failedEthereumTxLogRegex matches the ABCI log of a tx whose MsgEthereumTx failed in the EVM: the BaseApp
// runMsgs prefix followed by the abciLogVMError JSON that only the EVM msg server returns, ending the log.
// It is not anchored at the start because debug (trace) mode prepends a stack trace to the log.
var failedEthereumTxLogRegex = regexp.MustCompile(`failed to execute message; message index: (\d+): (\{"tx_hash":"0x[0-9a-fA-F]{64}","gas_used":\d+,.*\})$`)

// NormalizeTxResponseIndexes returns cloned tx results with EVM response log
// indexes normalized to account for failed Ethereum txs that still consume block
// tx slots.
func NormalizeTxResponseIndexes(input []*abci.ExecTxResult) ([]*abci.ExecTxResult, error) {
	if len(input) == 0 {
		return nil, nil
	}

	normalized := make([]*abci.ExecTxResult, len(input))
	for i, res := range input {
		if res == nil {
			continue
		}

		resCopy := *res
		normalized[i] = &resCopy
	}

	var (
		txIndex  uint64
		logIndex uint64
	)

	for _, res := range normalized {
		if res == nil {
			continue
		}

		ethTxCount := countEthereumTxEvents(res.Events)
		inferredEthTxCount := inferFailedEthereumTxCount(res.Codespace, res.Log)
		if inferredEthTxCount > ethTxCount {
			ethTxCount = inferredEthTxCount
		}

		if res.Code != abci.CodeTypeOK {
			txIndex += uint64(ethTxCount)
			continue
		}

		if len(res.Data) == 0 {
			txIndex += uint64(ethTxCount)
			continue
		}

		var txMsgData sdk.TxMsgData
		if err := proto.Unmarshal(res.Data, &txMsgData); err != nil {
			return nil, err
		}

		var (
			dataDirty bool
			seenEthTx uint64
		)

		for i, rsp := range txMsgData.MsgResponses {
			var response evmtypes.MsgEthereumTxResponse
			if rsp.TypeUrl != "/"+proto.MessageName(&response) {
				continue
			}

			seenEthTx++

			if err := proto.Unmarshal(rsp.Value, &response); err != nil {
				return nil, err
			}

			responseDirty := false
			for j := range response.Logs {
				if response.Logs[j].TxIndex != txIndex {
					response.Logs[j].TxIndex = txIndex
					responseDirty = true
				}
				if response.Logs[j].Index != logIndex {
					response.Logs[j].Index = logIndex
					responseDirty = true
				}
				logIndex++
			}

			if responseDirty {
				bz, err := proto.Marshal(&response)
				if err != nil {
					return nil, err
				}
				txMsgData.MsgResponses[i].Value = bz
				dataDirty = true
			}

			txIndex++
		}

		if seenEthTx < uint64(ethTxCount) {
			txIndex += uint64(ethTxCount) - seenEthTx
		}

		if dataDirty {
			data, err := proto.Marshal(&txMsgData)
			if err != nil {
				return nil, err
			}
			res.Data = data
		}
	}

	return normalized, nil
}

func countEthereumTxEvents(events []abci.Event) int {
	count := 0
	for _, event := range events {
		if event.Type == evmtypes.EventTypeEthereumTx {
			count++
		}
	}
	return count
}

// inferFailedEthereumTxCount returns the number of ethereum txs consumed by a tx that failed with an EVM error,
// or 0 when the failure is not an EVM one. Failed txs carry no msg responses or msg events, so only the log is left.
func inferFailedEthereumTxCount(codespace, log string) int {
	// VM errors carry the evm codespace, or the undefined one for reverts without a decodable reason.
	if codespace != evmtypes.ModuleName && codespace != errorsmod.UndefinedCodespace {
		return 0
	}

	match := failedEthereumTxLogRegex.FindStringSubmatch(log)
	if len(match) != 3 {
		return 0
	}

	var vmErr abciLogVMError
	if err := json.Unmarshal([]byte(match[2]), &vmErr); err != nil || vmErr.VMError == "" {
		return 0
	}

	msgIndex, err := strconv.Atoi(match[1])
	if err != nil || msgIndex < 0 {
		return 0
	}

	return msgIndex + 1
}
