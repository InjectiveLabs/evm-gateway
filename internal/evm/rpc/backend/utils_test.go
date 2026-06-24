package backend

import (
	"context"
	"errors"
	"math/big"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	proto "github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	txfeestypes "github.com/InjectiveLabs/sdk-go/chain/txfees/types"
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

func TestProcessBlockSetsNextBaseFeeToCurrentBaseFee(t *testing.T) {
	baseFee := big.NewInt(160000000)
	b := &Backend{
		queryClient: &rpctypes.QueryClient{
			TxFeesQueryClient: backendTestTxFeesQueryClient{err: errors.New("query unavailable")},
		},
		baseTraceTags: newBackendTraceTags(),
	}
	blockRes := &cmrpctypes.ResultBlockResults{
		Height:              1,
		FinalizeBlockEvents: []abci.Event{backendTestBaseFeeEvent(baseFee)},
	}
	cometBlock := &cmrpctypes.ResultBlock{
		Block: &cmtypes.Block{Header: cmtypes.Header{Height: 1}},
	}
	ethBlock := map[string]interface{}{
		"gasLimit": hexutil.Uint64(100000),
		"gasUsed":  (*hexutil.Big)(big.NewInt(21000)),
	}

	feeHistory := rpctypes.OneFeeHistory{}
	err := b.processBlock(cometBlock, ethBlock, nil, blockRes, &feeHistory)

	require.NoError(t, err)
	require.Equal(t, 0, feeHistory.BaseFee.Cmp(baseFee))
	require.Equal(t, 0, feeHistory.NextBaseFee.Cmp(baseFee))

	feeHistory.BaseFee.SetInt64(0)
	require.Equal(t, 0, feeHistory.NextBaseFee.Cmp(baseFee))
}

func TestEffectiveGasPriceUsesDynamicFeeFormula(t *testing.T) {
	baseFee := big.NewInt(160000000)
	feeCap := big.NewInt(1716000000)
	tipCap := big.NewInt(1500000000)
	expectedEffectiveGasPrice := big.NewInt(1660000000)
	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		GasFeeCap: feeCap,
		GasTipCap: tipCap,
		Gas:       21000,
	})

	got := effectiveGasPrice(tx, baseFee)

	require.Equal(t, 0, got.Cmp(expectedEffectiveGasPrice))
	require.NotEqual(t, 0, got.Cmp(feeCap))
	require.Equal(t, 0, effectiveGasPrice(tx, nil).Cmp(feeCap))
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

type backendTestTxFeesQueryClient struct {
	baseFee *txfeestypes.QueryEipBaseFeeResponse
	err     error
}

func (c backendTestTxFeesQueryClient) Params(
	context.Context,
	*txfeestypes.QueryParamsRequest,
	...grpc.CallOption,
) (*txfeestypes.QueryParamsResponse, error) {
	return nil, c.err
}

func (c backendTestTxFeesQueryClient) GetEipBaseFee(
	context.Context,
	*txfeestypes.QueryEipBaseFeeRequest,
	...grpc.CallOption,
) (*txfeestypes.QueryEipBaseFeeResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.baseFee, nil
}

func backendTestBaseFeeEvent(baseFee *big.Int) abci.Event {
	return abci.Event{
		Type: "txfees",
		Attributes: []abci.EventAttribute{
			{Key: "basefee", Value: baseFee.String()},
		},
	}
}
