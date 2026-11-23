package types

import (
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
)

// TxTraceResult is the result of a single transaction trace during a block trace.
type TxTraceResult struct {
	TxHash common.Hash `json:"txHash"`           // Hash of the traced transaction
	Result interface{} `json:"result,omitempty"` // Trace results produced by the tracer
	Error  string      `json:"error,omitempty"`  // Trace failure produced by the tracer
}

// MarshalJSON encodes an entry without error with its result, `null`
// included: a trace result is null for tracers whose output is not derived
// for txs that are not replayed, and indexers tell results from failures by
// the key.
func (r TxTraceResult) MarshalJSON() ([]byte, error) {
	if r.Error != "" {
		type entry TxTraceResult
		return json.Marshal(entry(r))
	}
	return json.Marshal(struct {
		TxHash common.Hash `json:"txHash"`
		Result interface{} `json:"result"`
	}{r.TxHash, r.Result})
}
