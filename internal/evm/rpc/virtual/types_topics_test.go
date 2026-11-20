package virtual

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// TestLogWithoutTopicsSerializesEmptyTopicsArray verifies a LOG0 log keeps
// "topics": [] on every conversion path; null breaks strict clients (IC-1346).
func TestLogWithoutTopicsSerializesEmptyTopicsArray(t *testing.T) {
	emitter := common.HexToAddress("0x69065dfaba632ace25e6bb4730e3235a04ea6750")
	rpcLog := NewRPCLog(&ethtypes.Log{Address: emitter, Data: []byte{0x01}}, false, nil)
	require.NotNil(t, rpcLog.Topics)

	for name, marshal := range map[string]func(any) ([]byte, error){
		"encoding/json": json.Marshal,
		"sonic":         sonic.Marshal,
	} {
		bz, err := marshal(rpcLog)
		require.NoError(t, err, name)
		require.Contains(t, string(bz), `"topics":[]`, name)
	}

	var decoded RPCLog
	require.NoError(t, decoded.UnmarshalJSON([]byte(`{"address":"`+emitter.Hex()+`","topics":null,"data":"0x01"}`)))
	require.NotNil(t, decoded.Topics)
	require.Empty(t, decoded.Topics)

	bz, err := json.Marshal(EthLog(&RPCLog{Address: emitter}))
	require.NoError(t, err)
	require.Contains(t, string(bz), `"topics":[]`)
}
