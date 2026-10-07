package virtual

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	virtualbank "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/bank"
	virtualibc "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/ibc"
)

type entry struct {
	logs     []*ethtypes.Log
	msgIndex *int
	hook     *virtualibc.HookCall
}

// Response is an ordered, source-neutral view of the virtualizable events in
// one full Cosmos transaction response.
type Response struct {
	entries []entry
	code    uint32
	gasUsed uint64
}

// TxContext supplies RPC identity that is not part of an ABCI transaction
// response.
type TxContext struct {
	Tx                      cmtypes.Tx
	EthereumMessageIndexes  map[int]bool
	EthereumGasUsed         uint64
	TotalMessages           int
	BlockHash               common.Hash
	BlockNumber             uint64
	TxIndex                 uint64
	FirstLogIndex           uint
	CumulativeGasUsedBefore uint64
	ChainID                 *big.Int
}

// ParseResponse parses every supported virtual event from a full ABCI response.
func ParseResponse(resp *types.ExecTxResult) (*Response, error) {
	if resp == nil {
		return nil, fmt.Errorf("transaction response is nil")
	}

	out := &Response{code: resp.Code}
	if resp.GasUsed > 0 {
		out.gasUsed = uint64(resp.GasUsed)
	}

	for eventIndex, event := range resp.Events {
		bankEvents, matched, err := virtualbank.ParseEvent(event, eventIndex)
		if err != nil {
			return nil, err
		}

		if matched {
			logs, err := virtualbank.EthereumLogs(bankEvents)
			if err != nil {
				return nil, fmt.Errorf("bank event %d: %w", eventIndex, err)
			}

			var msgIndex *int
			if len(bankEvents) > 0 {
				msgIndex = bankEvents[0].MsgIndex
			}

			out.entries = append(out.entries, entry{logs: logs, msgIndex: msgIndex})
			continue
		}

		hook, matched, err := virtualibc.ParseEvent(event, eventIndex)
		if err != nil {
			return nil, err
		}

		if matched {
			logs, err := virtualibc.EthereumLogs(hook)
			if err != nil {
				return nil, fmt.Errorf("IBC hook event %d: %w", eventIndex, err)
			}

			out.entries = append(out.entries, entry{logs: logs, msgIndex: hook.MsgIndex, hook: hook})
		}
	}

	return out, nil
}

// LogsForMessage returns virtual logs belonging to a genuine MsgEthereumTx.
func (r *Response) LogsForMessage(msgIndex, totalMessages int, ctx LogContext) []*RPCLog {
	logs := make([]*RPCLog, 0)
	for _, event := range r.entries {
		if belongsToMessage(event.msgIndex, msgIndex, totalMessages) {
			eventLogs := WrapLogs(event.logs, true, nil)
			if event.hook != nil {
				for _, log := range eventLogs {
					log.CosmosMsgIndex = hookMsgIndex(event.hook)
				}
			}
			logs = append(logs, eventLogs...)
		}
	}

	SetLogMetadata(logs, ctx)
	return logs
}

// SyntheticTxs preserves the transaction-wide bank projection and gives every
// non-Ethereum IBC hook its own transaction identified by the message index.
func (r *Response) SyntheticTxs(ctx TxContext) ([]*Tx, error) {
	bankEvents := make([]entry, 0)
	hooks := make([]entry, 0)
	for _, event := range r.entries {
		if len(event.logs) == 0 {
			continue
		}
		if !belongsToNonEthereumMessage(event.msgIndex, ctx.EthereumMessageIndexes, ctx.TotalMessages) {
			continue
		}

		if event.hook != nil {
			hooks = append(hooks, event)
		} else {
			bankEvents = append(bankEvents, event)
		}
	}

	if len(bankEvents) == 0 && len(hooks) == 0 {
		return nil, nil
	}

	if len(hooks) == 0 {
		return []*Tx{r.syntheticTx(ctx, bankEvents, nil, CosmosTxHash(ctx.Tx), r.gasUsed)}, nil
	}

	// A message index is necessary to derive a stable identity. Do not guess
	// indexes for ambiguous responses or silently overwrite duplicate hashes.
	seen := make(map[int]bool, len(hooks))
	for _, event := range hooks {
		if event.msgIndex == nil || *event.msgIndex < 0 || *event.msgIndex >= ctx.TotalMessages {
			return nil, fmt.Errorf("IBC hook calls require valid msg_index attributes")
		}
		if seen[*event.msgIndex] {
			return nil, fmt.Errorf("multiple IBC hook calls for msg_index %d", *event.msgIndex)
		}
		seen[*event.msgIndex] = true
	}
	sort.Slice(hooks, func(i, j int) bool { return *hooks[i].msgIndex < *hooks[j].msgIndex })
	ctx.CumulativeGasUsedBefore += ctx.EthereumGasUsed

	// Allocate the remaining Cosmos gas to the bank envelope. Hook gas and
	// genuine Ethereum message gas are accounted for separately, exactly once.
	bankGasUsed := r.gasUsed
	accountGas := func(gas uint64) {
		if gas >= bankGasUsed {
			bankGasUsed = 0
		} else {
			bankGasUsed -= gas
		}
	}
	accountGas(ctx.EthereumGasUsed)
	for _, event := range hooks {
		accountGas(event.hook.GasUsed)
	}

	txs := make([]*Tx, 0, len(hooks)+1)
	appendTx := func(tx *Tx) {
		txs = append(txs, tx)
		ctx.TxIndex++
		ctx.FirstLogIndex += uint(len(tx.Receipt.Logs))
		ctx.CumulativeGasUsedBefore = tx.Receipt.CumulativeGasUsed
	}
	if len(bankEvents) > 0 {
		appendTx(r.syntheticTx(ctx, bankEvents, nil, CosmosTxHash(ctx.Tx), bankGasUsed))
	}
	for _, event := range hooks {
		hash := IBCHookTxHash(ctx.Tx, *event.msgIndex)
		appendTx(r.syntheticTx(ctx, []entry{event}, event.hook, hash, event.hook.GasUsed))
	}
	return txs, nil
}

func (r *Response) syntheticTx(ctx TxContext, events []entry, hook *virtualibc.HookCall, txHash common.Hash, gasUsed uint64) *Tx {
	cosmosHash := OriginalCosmosTxHash(ctx.Tx)
	logCtx := LogContext{
		BlockHash:     ctx.BlockHash,
		BlockNumber:   ctx.BlockNumber,
		TxHash:        txHash,
		TxIndex:       uint(ctx.TxIndex),
		FirstLogIndex: ctx.FirstLogIndex,
		CosmosHash:    &cosmosHash,
	}

	logs := make([]*RPCLog, 0)
	for _, event := range events {
		eventLogs := WrapLogs(event.logs, true, &cosmosHash)
		if event.hook != nil {
			for _, log := range eventLogs {
				log.CosmosMsgIndex = hookMsgIndex(event.hook)
			}
		}
		logs = append(logs, eventLogs...)
	}
	SetLogMetadata(logs, logCtx)

	var (
		from           = common.Address{}
		to             = virtualbank.ContractAddress
		input          = []byte{}
		txGas          = uint64(0)
		receiptGasUsed = gasUsed
		status         = uint64(ethtypes.ReceiptStatusSuccessful)
		vmError        = ""
	)

	if r.code != types.CodeTypeOK {
		status = uint64(ethtypes.ReceiptStatusFailed)
	}

	if hook != nil {
		from = virtualibc.ContractAddress
		to = hook.Contract
		input = hook.Input
		txGas = hook.GasUsed
		receiptGasUsed = hook.GasUsed
		vmError = hook.Error

		if !hook.Success {
			status = uint64(ethtypes.ReceiptStatusFailed)
		}
	}

	tx := newRPCTransaction(
		txHash, ctx.BlockHash, ctx.BlockNumber, ctx.TxIndex, ctx.ChainID,
		&cosmosHash, from, to, input, txGas,
	)
	if hook != nil {
		tx.CosmosMsgIndex = hookMsgIndex(hook)
	}

	receipt := &Receipt{
		Status:            status,
		CumulativeGasUsed: ctx.CumulativeGasUsedBefore + receiptGasUsed,
		GasUsed:           receiptGasUsed,
		VMError:           vmError,
		Logs:              logs,
		TransactionHash:   txHash,
		BlockHash:         ctx.BlockHash,
		BlockNumber:       ctx.BlockNumber,
		TransactionIndex:  ctx.TxIndex,
		EffectiveGasPrice: big.NewInt(0),
		From:              from,
		To:                &to,
		Type:              uint64(ethtypes.LegacyTxType),
	}

	return &Tx{Transaction: tx, Receipt: receipt}
}

func hookMsgIndex(hook *virtualibc.HookCall) *hexutil.Uint64 {
	if hook.MsgIndex != nil {
		index := hexutil.Uint64(*hook.MsgIndex)
		return &index
	}
	return nil
}

// SplitBlockEvents parses virtualizable FinalizeBlock events and separates the
// bank begin/end phases. IBC hook events are transaction-scoped and ignored.
func SplitBlockEvents(events []types.Event) (begin, end []*ethtypes.Log, err error) {
	bankEvents, err := virtualbank.ParseEvents(events)
	if err != nil {
		return nil, nil, err
	}

	beginEvents := make([]virtualbank.Event, 0)
	endEvents := make([]virtualbank.Event, 0)
	for _, event := range bankEvents {
		if event.Mode == virtualbank.ModeBeginBlock {
			beginEvents = append(beginEvents, event)
		} else {
			endEvents = append(endEvents, event)
		}
	}

	begin, err = virtualbank.EthereumLogs(beginEvents)
	if err != nil {
		return nil, nil, err
	}

	end, err = virtualbank.EthereumLogs(endEvents)
	return begin, end, err
}

// MaterializeLogs assigns RPC identity to metadata-free virtual logs.
func MaterializeLogs(logs []*ethtypes.Log, ctx LogContext) []*RPCLog {
	out := WrapLogs(logs, true, ctx.CosmosHash)
	SetLogMetadata(out, ctx)

	return out
}

// NewBlockTx constructs a bank-only synthetic transaction for begin/end block
// virtual events.
func NewBlockTx(
	hash common.Hash,
	logs []*ethtypes.Log,
	ctx LogContext,
	chainID *big.Int,
	cumulativeGasUsed uint64,
) *Tx {
	materialized := MaterializeLogs(logs, ctx)
	to := virtualbank.ContractAddress

	tx := newRPCTransaction(
		hash,
		ctx.BlockHash,
		ctx.BlockNumber,
		uint64(ctx.TxIndex),
		chainID,
		nil,
		common.Address{},
		to,
		nil,
		0,
	)

	return &Tx{
		Transaction: tx,
		Receipt: &Receipt{
			Status:            ethtypes.ReceiptStatusSuccessful,
			CumulativeGasUsed: cumulativeGasUsed,
			Logs:              materialized,
			TransactionHash:   hash,
			BlockHash:         ctx.BlockHash,
			BlockNumber:       ctx.BlockNumber,
			TransactionIndex:  uint64(ctx.TxIndex),
			EffectiveGasPrice: big.NewInt(0),
			From:              common.Address{},
			To:                &to,
			Type:              uint64(ethtypes.LegacyTxType),
		},
	}
}

func belongsToMessage(msgIndex *int, wanted, total int) bool {
	if msgIndex != nil {
		return *msgIndex == wanted
	}

	return total == 1 && wanted == 0
}

func belongsToNonEthereumMessage(msgIndex *int, ethereumIndexes map[int]bool, total int) bool {
	if msgIndex != nil {
		return !ethereumIndexes[*msgIndex]
	}

	return !(total == 1 && ethereumIndexes[0])
}
