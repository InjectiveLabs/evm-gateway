package backend

import (
	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// Ethereum txs whose Cosmos tx failed in the ante handler (e.g. insufficient
// funds for the transferred value) are part of the Comet block and are exposed
// with a failed receipt, but they were never executed. Replaying them in a
// trace fails before execution and the node returns an error such as
// "insufficient balance for transfer", which clients expect only for txs that
// can't be traced at all. For those txs the gateway returns a trace that
// matches the failed receipt instead: a top-level frame that consumed the gas
// limit and carries the ante handler error, without any execution.

const (
	tracerCall = "callTracer"
	// tracerStructLogger is the default tracer, selected by an empty name.
	tracerStructLogger = ""
)

// anteFailedCallFrame is the callTracer frame of a tx that was not executed.
type anteFailedCallFrame struct {
	Type    string          `json:"type"`
	From    common.Address  `json:"from"`
	To      *common.Address `json:"to,omitempty"`
	Value   *hexutil.Big    `json:"value"`
	Gas     hexutil.Uint64  `json:"gas"`
	GasUsed hexutil.Uint64  `json:"gasUsed"`
	Input   hexutil.Bytes   `json:"input"`
	Error   string          `json:"error"`
}

// anteFailedStructLog is the default (struct logger) trace of a tx that was
// not executed.
type anteFailedStructLog struct {
	Gas         uint64        `json:"gas"`
	Failed      bool          `json:"failed"`
	ReturnValue string        `json:"returnValue"`
	StructLogs  []interface{} `json:"structLogs"`
}

// anteFailedTraceResult builds the trace of an ante-failed Ethereum message for
// the tracers that have a meaningful empty execution. The reported gas used is
// the gas limit, like the tx receipt. Other tracers keep the node error.
func anteFailedTraceResult(msg *evmtypes.MsgEthereumTx, res *abci.ExecTxResult, config *rpctypes.TraceConfig) (interface{}, bool) {
	if msg == nil || res == nil {
		return nil, false
	}
	txData := msg.AsTransaction()
	if txData == nil {
		return nil, false
	}

	tracer := tracerStructLogger
	if config != nil {
		tracer = config.Tracer
	}

	switch tracer {
	case tracerCall:
		from, ok := anteFailedSender(msg, txData)
		if !ok {
			return nil, false
		}
		frame := &anteFailedCallFrame{
			Type:    "CALL",
			From:    from,
			To:      txData.To(),
			Value:   (*hexutil.Big)(txData.Value()),
			Gas:     hexutil.Uint64(txData.Gas()),
			GasUsed: hexutil.Uint64(txData.Gas()),
			Input:   txData.Data(),
			Error:   res.Log,
		}
		if frame.To == nil {
			frame.Type = "CREATE"
		}
		return frame, true
	case tracerStructLogger:
		return &anteFailedStructLog{
			Gas:         txData.Gas(),
			Failed:      true,
			ReturnValue: "",
			StructLogs:  []interface{}{},
		}, true
	default:
		return nil, false
	}
}

func anteFailedSender(msg *evmtypes.MsgEthereumTx, txData *ethtypes.Transaction) (common.Address, bool) {
	if len(msg.From) > 0 {
		return common.BytesToAddress(msg.From), true
	}
	var signer ethtypes.Signer
	if txData.Protected() {
		signer = ethtypes.LatestSignerForChainID(txData.ChainId())
	} else {
		signer = ethtypes.FrontierSigner{}
	}
	from, err := msg.GetSenderLegacy(signer)
	if err != nil {
		return common.Address{}, false
	}
	return from, true
}

// anteFailedTxTrace returns the trace of an Ethereum tx whose Cosmos tx failed
// in the ante handler, or false when the tx doesn't qualify or can't be
// resolved. It only runs after a trace attempt failed, so successful traces pay
// no extra lookups.
func (b *Backend) anteFailedTxTrace(hash common.Hash, config *rpctypes.TraceConfig) (interface{}, bool) {
	if b.cfg.OfflineRPCOnly {
		return nil, false
	}
	tx, err := b.GetTxByEthHash(hash)
	if err != nil || tx == nil || tx.Height <= 0 {
		return nil, false
	}

	blockRes, err := b.TendermintBlockResultByNumber(&tx.Height)
	if err != nil || blockRes == nil || int(tx.TxIndex) >= len(blockRes.TxResults) {
		return nil, false
	}
	res := blockRes.TxResults[tx.TxIndex]
	if !rpctypes.TxAnteFailed(res) {
		return nil, false
	}

	block, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(tx.Height))
	if err != nil || block == nil || block.Block == nil || int(tx.TxIndex) >= len(block.Block.Txs) {
		return nil, false
	}
	decodedTx, err := b.clientCtx.TxConfig.TxDecoder()(block.Block.Txs[tx.TxIndex])
	if err != nil {
		return nil, false
	}
	msgs := decodedTx.GetMsgs()
	if int(tx.MsgIndex) >= len(msgs) {
		return nil, false
	}
	msg, ok := msgs[tx.MsgIndex].(*evmtypes.MsgEthereumTx)
	if !ok || msg.Hash() != hash {
		return nil, false
	}

	result, ok := anteFailedTraceResult(msg, res, config)
	if ok {
		b.logger.Debug("returning trace of an ante-failed tx", "hash", hash.Hex(), "height", tx.Height, "error", res.Log)
	}
	return result, ok
}

// fillAnteFailedBlockTraces replaces the error entries of a block trace that
// belong to ante-failed txs with their trace. The block trace has one entry per
// MsgEthereumTx of the block, in block order, as sent to the node.
func (b *Backend) fillAnteFailedBlockTraces(
	results []*rpctypes.TxTraceResult,
	height rpctypes.BlockNumber,
	block *cmrpctypes.ResultBlock,
	config *rpctypes.TraceConfig,
) {
	if b.cfg.OfflineRPCOnly || !hasTraceErrors(results) {
		return
	}
	if block == nil || block.Block == nil {
		var err error
		block, err = b.TendermintBlockByNumber(height)
		if err != nil || block == nil || block.Block == nil {
			return
		}
	}
	blockHeight := block.Block.Height
	blockRes, err := b.TendermintBlockResultByNumber(&blockHeight)
	if err != nil || blockRes == nil {
		return
	}

	type position struct {
		msg *evmtypes.MsgEthereumTx
		res *abci.ExecTxResult
	}
	positions := make([]position, 0, len(results))
	txDecoder := b.clientCtx.TxConfig.TxDecoder()
	for txIndex, txBz := range block.Block.Txs {
		decodedTx, err := txDecoder(txBz)
		if err != nil {
			// skipped when the block trace request was built, too
			continue
		}
		var res *abci.ExecTxResult
		if txIndex < len(blockRes.TxResults) {
			res = blockRes.TxResults[txIndex]
		}
		for _, msg := range decodedTx.GetMsgs() {
			if ethMsg, ok := msg.(*evmtypes.MsgEthereumTx); ok {
				positions = append(positions, position{msg: ethMsg, res: res})
			}
		}
	}
	if len(positions) != len(results) {
		b.logger.Debug("block trace does not match the block's ethereum txs; leaving error entries", "height", blockHeight, "results", len(results), "txs", len(positions))
		return
	}

	for i, result := range results {
		if result == nil || result.Error == "" || !rpctypes.TxAnteFailed(positions[i].res) {
			continue
		}
		trace, ok := anteFailedTraceResult(positions[i].msg, positions[i].res, config)
		if !ok {
			continue
		}
		results[i] = &rpctypes.TxTraceResult{Result: trace}
	}
}

func hasTraceErrors(results []*rpctypes.TxTraceResult) bool {
	for _, result := range results {
		if result != nil && result.Error != "" {
			return true
		}
	}
	return false
}
