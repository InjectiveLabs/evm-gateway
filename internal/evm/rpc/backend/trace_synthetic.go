package backend

import (
	"math/big"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

const (
	callTracerName     = "callTracer"
	prestateTracerName = "prestateTracer"

	// cosmosTxFailedError is the frame error of a failed virtual tx whose
	// receipt carries no VM error.
	cosmosTxFailedError = "cosmos transaction failed"
)

// syntheticCallFrame is a top-level callTracer frame, encoded like geth's.
type syntheticCallFrame struct {
	From         common.Address  `json:"from"`
	Gas          hexutil.Uint64  `json:"gas"`
	GasUsed      hexutil.Uint64  `json:"gasUsed"`
	To           *common.Address `json:"to,omitempty"`
	Input        hexutil.Bytes   `json:"input"`
	Error        string          `json:"error,omitempty"`
	RevertReason string          `json:"revertReason,omitempty"`
	Logs         []syntheticLog  `json:"logs,omitempty"`
	Value        *hexutil.Big    `json:"value,omitempty"`
	Type         string          `json:"type"`
}

// syntheticLog is a callTracer log (withLog), encoded like geth's.
type syntheticLog struct {
	Address  common.Address `json:"address"`
	Topics   []common.Hash  `json:"topics"`
	Data     hexutil.Bytes  `json:"data"`
	Position hexutil.Uint   `json:"position"`
}

// syntheticStructLog is a struct logger result without opcodes, encoded like
// geth's.
type syntheticStructLog struct {
	Gas         uint64        `json:"gas"`
	Failed      bool          `json:"failed"`
	ReturnValue hexutil.Bytes `json:"returnValue"`
	StructLogs  []struct{}    `json:"structLogs"`
}

// syntheticReceipt holds the receipt fields a synthetic trace is built from.
type syntheticReceipt struct {
	Status          hexutil.Uint64  `json:"status"`
	GasUsed         hexutil.Uint64  `json:"gasUsed"`
	ContractAddress *common.Address `json:"contractAddress"`
	VMError         string          `json:"vmError"`
	Reason          string          `json:"reason"`
	Logs            []struct {
		Address common.Address `json:"address"`
		Topics  []common.Hash  `json:"topics"`
		Data    hexutil.Bytes  `json:"data"`
	} `json:"logs"`
}

// syntheticTraceResult returns the trace of a visible tx that cannot be
// replayed, built from the tx and receipt served over JSON-RPC, in the shape
// of the requested tracer:
//
//   - callTracer: one top-level CALL (or CREATE) frame without inner calls;
//   - struct logger: the gas used and status, without opcodes;
//   - prestateTracer: no accounts;
//   - other tracers: nil, as their output cannot be derived.
//
// errMsg, when set, is the frame error and marks the tx as failed. Otherwise
// the receipt status decides.
func syntheticTraceResult(
	tx *rpctypes.RPCTransaction,
	receiptMap map[string]interface{},
	config *rpctypes.TraceConfig,
	errMsg string,
) (interface{}, error) {
	var receipt syntheticReceipt
	bz, err := sonic.Marshal(receiptMap)
	if err != nil {
		return nil, err
	}
	if err := sonic.Unmarshal(bz, &receipt); err != nil {
		return nil, err
	}

	failed := errMsg != "" || receipt.Status == 0
	if failed && errMsg == "" {
		errMsg = receipt.VMError
		if errMsg == "" {
			errMsg = cosmosTxFailedError
		}
	}

	var tracer string
	var tracerConfig struct {
		WithLog  bool `json:"withLog"`
		DiffMode bool `json:"diffMode"`
	}
	if config != nil {
		tracer = config.Tracer
		if len(config.TracerConfig) > 0 {
			// geth ignores unknown fields; an invalid config fails upstream
			// for replayed txs, the synthetic result keeps the defaults
			_ = sonic.Unmarshal(config.TracerConfig, &tracerConfig)
		}
	}

	switch tracer {
	case "":
		return syntheticStructLog{
			Gas:         uint64(receipt.GasUsed),
			Failed:      failed,
			ReturnValue: hexutil.Bytes{},
			StructLogs:  []struct{}{},
		}, nil
	case prestateTracerName:
		if tracerConfig.DiffMode {
			return map[string]interface{}{"post": map[string]interface{}{}, "pre": map[string]interface{}{}}, nil
		}
		return map[string]interface{}{}, nil
	case callTracerName:
	default:
		return nil, nil
	}

	value := tx.Value
	if value == nil {
		value = (*hexutil.Big)(new(big.Int))
	}
	frame := syntheticCallFrame{
		Type:    "CALL",
		From:    tx.From,
		To:      tx.To,
		Gas:     tx.Gas,
		GasUsed: receipt.GasUsed,
		Input:   tx.Input,
		Value:   value,
	}
	if frame.Input == nil {
		frame.Input = hexutil.Bytes{}
	}
	if tx.To == nil {
		frame.Type = "CREATE"
		to := crypto.CreateAddress(tx.From, uint64(tx.Nonce))
		if receipt.ContractAddress != nil {
			to = *receipt.ContractAddress
		}
		frame.To = &to
	}
	if failed {
		frame.Error = errMsg
		frame.RevertReason = receipt.Reason
	} else if tracerConfig.WithLog {
		// geth drops the logs of failed frames
		for _, log := range receipt.Logs {
			frame.Logs = append(frame.Logs, syntheticLog{Address: log.Address, Topics: log.Topics, Data: log.Data})
		}
	}
	return frame, nil
}

// isLegacyTracePlaceholder reports whether a trace entry has the shape of the
// `{"type": 0}` placeholder that v1.4.3 and v1.4.4 cached for virtual txs. A
// custom tracer can return the same JSON, so the shape alone does not identify
// a placeholder; see dropLegacyTracePlaceholders.
func isLegacyTracePlaceholder(entry *rpctypes.TxTraceResult) bool {
	if entry == nil || entry.Error != "" {
		return false
	}
	result, ok := entry.Result.(map[string]interface{})
	if !ok || len(result) != 1 {
		return false
	}
	switch v := result["type"].(type) {
	case float64:
		return v == 0
	case int:
		return v == 0
	case int64:
		return v == 0
	default:
		return false
	}
}

// dropLegacyTracePlaceholders removes from cached block trace entries the
// placeholders that v1.4.3 and v1.4.4 wrote for virtual txs, so that the
// alignment rebuilds them as synthetic traces. Virtual txs are never replayed,
// so a placeholder-shaped entry of a virtual tx cannot be a tracer result;
// entries of other txs are kept, whatever a custom tracer returned for them.
func (b *Backend) dropLegacyTracePlaceholders(results []*rpctypes.TxTraceResult) []*rpctypes.TxTraceResult {
	lookup, ok := b.indexer.(virtualRPCTransactionLookup)
	if !ok {
		return results
	}
	kept := make([]*rpctypes.TxTraceResult, 0, len(results))
	for _, entry := range results {
		if isLegacyTracePlaceholder(entry) {
			if virtual, err := lookup.IsVirtualRPCTransaction(entry.TxHash); err == nil && virtual {
				continue
			}
		}
		kept = append(kept, entry)
	}
	return kept
}
