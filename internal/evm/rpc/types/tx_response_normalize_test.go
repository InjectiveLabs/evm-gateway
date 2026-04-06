package types

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	proto "github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestInferFailedEthereumTxCount(t *testing.T) {
	testCases := []struct {
		name  string
		log   string
		count int
	}{
		{
			name:  "vm error payload",
			log:   `failed to execute message; message index: 0: {"tx_hash":"0xabc","gas_used":21000,"vm_error":"execution reverted"}`,
			count: 1,
		},
		{
			name:  "non vm error payload",
			log:   `failed to execute message; message index: 1: {"tx_hash":"0xabc","gas_used":21000,"reason":"insufficient funds"}`,
			count: 2,
		},
		{
			name:  "out of gas log with ethereum message marker",
			log:   `failed to execute message; message index: 0: out of gas in location: WritePerByte (/injective.evm.v1.MsgEthereumTx)`,
			count: 1,
		},
		{
			name:  "no message index",
			log:   `failed to execute message: {"tx_hash":"0xabc","gas_used":21000,"reason":"insufficient funds"}`,
			count: 0,
		},
		{
			name:  "non ethereum message index log",
			log:   `failed to execute message; message index: 0: insufficient funds for message /cosmos.bank.v1beta1.MsgSend`,
			count: 0,
		},
		{
			name:  "malformed message index",
			log:   `failed to execute message; message index: abc: {"tx_hash":"0xabc","gas_used":21000}`,
			count: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.count, inferFailedEthereumTxCount(tc.log))
		})
	}
}

func TestNormalizeTxResponseIndexesFailedEthereumTxWithoutVMErrorConsumesTxIndex(t *testing.T) {
	failed := &abci.ExecTxResult{
		Code: 1,
		Log:  `failed to execute message; message index: 0: {"tx_hash":"0xabc","gas_used":21000,"reason":"insufficient funds"}`,
	}

	success := &abci.ExecTxResult{
		Code: 0,
		Data: mustMarshalTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
			Hash: "0xdef",
			Logs: []*evmtypes.Log{{
				TxHash:  "0xdef",
				TxIndex: 0,
				Index:   0,
			}},
		}),
		Events: []abci.Event{{
			Type: evmtypes.EventTypeEthereumTx,
		}},
	}

	normalized, err := NormalizeTxResponseIndexes([]*abci.ExecTxResult{failed, success})
	require.NoError(t, err)

	logs, err := evmtypes.DecodeMsgLogs(normalized[1].Data, 0, 1)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.EqualValues(t, 1, logs[0].TxIndex)
	require.EqualValues(t, 0, logs[0].Index)
}

func TestNormalizeTxResponseIndexesMixedEventsAndRevertUsesHigherCount(t *testing.T) {
	failed := &abci.ExecTxResult{
		Code: 1,
		Events: []abci.Event{{
			Type: evmtypes.EventTypeEthereumTx,
		}},
		Log: `failed to execute message; message index: 1: {"tx_hash":"0xabc","reason":"reverted"}`,
	}

	success := &abci.ExecTxResult{
		Code: 0,
		Data: mustMarshalTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
			Hash: "0xdef",
			Logs: []*evmtypes.Log{{
				TxHash:  "0xdef",
				TxIndex: 0,
				Index:   0,
			}},
		}),
		Events: []abci.Event{{
			Type: evmtypes.EventTypeEthereumTx,
		}},
	}

	normalized, err := NormalizeTxResponseIndexes([]*abci.ExecTxResult{failed, success})
	require.NoError(t, err)

	logs, err := evmtypes.DecodeMsgLogs(normalized[1].Data, 0, 1)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.EqualValues(t, 2, logs[0].TxIndex)
}

func TestNormalizeTxResponseIndexesRewritesSequentialLogIndexesAcrossResults(t *testing.T) {
	first := &abci.ExecTxResult{
		Code: 0,
		Data: mustMarshalTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
			Hash: "0xaaa",
			Logs: []*evmtypes.Log{{
				TxHash:  "0xaaa",
				TxIndex: 9,
				Index:   9,
			}},
		}),
		Events: []abci.Event{{Type: evmtypes.EventTypeEthereumTx}},
	}
	failed := &abci.ExecTxResult{
		Code: 1,
		Log:  `failed to execute message; message index: 0: {"tx_hash":"0xbbb","reason":"reverted"}`,
	}
	second := &abci.ExecTxResult{
		Code: 0,
		Data: mustMarshalTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
			Hash: "0xccc",
			Logs: []*evmtypes.Log{
				{
					TxHash:  "0xccc",
					TxIndex: 0,
					Index:   0,
				},
				{
					TxHash:  "0xccc",
					TxIndex: 0,
					Index:   0,
				},
			},
		}),
		Events: []abci.Event{{Type: evmtypes.EventTypeEthereumTx}},
	}

	normalized, err := NormalizeTxResponseIndexes([]*abci.ExecTxResult{first, failed, second})
	require.NoError(t, err)

	firstLogs, err := evmtypes.DecodeMsgLogs(normalized[0].Data, 0, 1)
	require.NoError(t, err)
	require.Len(t, firstLogs, 1)
	require.EqualValues(t, 0, firstLogs[0].TxIndex)
	require.EqualValues(t, 0, firstLogs[0].Index)

	secondLogs, err := evmtypes.DecodeMsgLogs(normalized[2].Data, 0, 1)
	require.NoError(t, err)
	require.Len(t, secondLogs, 2)
	require.EqualValues(t, 2, secondLogs[0].TxIndex)
	require.EqualValues(t, 1, secondLogs[0].Index)
	require.EqualValues(t, 2, secondLogs[1].TxIndex)
	require.EqualValues(t, 2, secondLogs[1].Index)
}

func mustMarshalTxMsgData(t *testing.T, responses ...*evmtypes.MsgEthereumTxResponse) []byte {
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
