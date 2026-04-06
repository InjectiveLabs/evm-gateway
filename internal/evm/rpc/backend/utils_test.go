package backend

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	proto "github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestGetLogsFromBlockResultsPatchesShiftedTransactionIndex(t *testing.T) {
	blockRes := &cmrpctypes.ResultBlockResults{
		Height: 42,
		TxResults: []*abci.ExecTxResult{
			{
				Code: 1,
				Log:  `failed to execute message; message index: 0: {"tx_hash":"0xabc","reason":"reverted"}`,
			},
			{
				Code: 0,
				Data: mustMarshalBackendTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
					Hash: "0xdef",
					Logs: []*evmtypes.Log{{
						TxHash:  "0xdef",
						TxIndex: 0,
						Index:   0,
					}},
				}),
				Events: []abci.Event{{Type: evmtypes.EventTypeEthereumTx}},
			},
		},
	}

	logGroups, err := GetLogsFromBlockResults(blockRes)
	require.NoError(t, err)
	require.Len(t, logGroups, 2)
	require.Len(t, logGroups[1], 1)
	require.EqualValues(t, 1, logGroups[1][0].TxIndex)
	require.EqualValues(t, 0, logGroups[1][0].Index)
}

func mustMarshalBackendTxMsgData(t *testing.T, responses ...*evmtypes.MsgEthereumTxResponse) []byte {
	t.Helper()

	msgResponses := make([]*codectypes.Any, 0, len(responses))
	for _, response := range responses {
		anyRsp, err := codectypes.NewAnyWithValue(response)
		require.NoError(t, err)
		msgResponses = append(msgResponses, anyRsp)
	}

	txMsgData := sdk.TxMsgData{MsgResponses: msgResponses}
	data, err := proto.Marshal(&txMsgData)
	require.NoError(t, err)
	return data
}
