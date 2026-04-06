package types

import (
	"regexp"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	proto "github.com/cosmos/gogoproto/proto"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

var msgIndexRegex = regexp.MustCompile(`message index:\s*(\d+)`)
var failedEthereumTxMarkerRegex = regexp.MustCompile(`"tx_hash"|"txHash"|"vm_error"|"vmError"|MsgEthereumTx|/injective\.evm\.v1\.MsgEthereumTx|ethereum_tx`)

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
		inferredEthTxCount := inferFailedEthereumTxCount(res.Log)
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

func inferFailedEthereumTxCount(log string) int {
	match := msgIndexRegex.FindStringSubmatch(log)
	if len(match) != 2 {
		return 0
	}
	if !failedEthereumTxMarkerRegex.MatchString(log) {
		return 0
	}

	msgIndex, err := strconv.Atoi(match[1])
	if err != nil || msgIndex < 0 {
		return 0
	}

	return msgIndex + 1
}
