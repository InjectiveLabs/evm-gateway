package types

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

// TestTxTraceResultJSON keeps the result key of entries without error, null
// included, with encoding/json (JSON-RPC responses) and sonic (trace cache).
func TestTxTraceResultJSON(t *testing.T) {
	hash := common.HexToHash("0x01")
	const hashJSON = `"0x0000000000000000000000000000000000000000000000000000000000000001"`
	for _, tc := range []struct {
		entry TxTraceResult
		want  string
	}{
		{TxTraceResult{TxHash: hash}, `{"txHash":` + hashJSON + `,"result":null}`},
		{TxTraceResult{TxHash: hash, Result: map[string]interface{}{"type": "CALL"}}, `{"txHash":` + hashJSON + `,"result":{"type":"CALL"}}`},
		{TxTraceResult{TxHash: hash, Error: "execution timeout"}, `{"txHash":` + hashJSON + `,"error":"execution timeout"}`},
	} {
		for name, marshal := range map[string]func(interface{}) ([]byte, error){"json": json.Marshal, "sonic": sonic.Marshal} {
			bz, err := marshal([]*TxTraceResult{&tc.entry})
			require.NoError(t, err)
			require.JSONEq(t, "["+tc.want+"]", string(bz), name)

			var decoded []*TxTraceResult
			require.NoError(t, sonic.Unmarshal(bz, &decoded))
			again, err := marshal(decoded)
			require.NoError(t, err)
			require.JSONEq(t, "["+tc.want+"]", string(again), name+" round trip")
		}
	}
}
