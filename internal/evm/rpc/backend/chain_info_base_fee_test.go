package backend

import (
	"errors"
	"math/big"
	"testing"

	sdkmath "cosmossdk.io/math"
	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/stretchr/testify/require"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	txfeestypes "github.com/InjectiveLabs/sdk-go/chain/txfees/types"
)

func newBaseFeeTestBackend(queryClient backendTestTxFeesQueryClient) *Backend {
	return &Backend{
		queryClient:   &rpctypes.QueryClient{TxFeesQueryClient: queryClient},
		baseTraceTags: newBackendTraceTags(),
	}
}

func eipBaseFeeResponse(baseFee int64) *txfeestypes.QueryEipBaseFeeResponse {
	return &txfeestypes.QueryEipBaseFeeResponse{
		BaseFee: &txfeestypes.EipBaseFee{BaseFee: sdkmath.LegacyNewDec(baseFee)},
	}
}

// txfees answers GetEipBaseFee from memory for any height within two of the
// head, so the query can already hold a later block's base fee. The block's own
// BeginBlocker event must win, or the same block reports different base fees
// depending on how close to the head it is when queried.
func TestBaseFeePrefersBlockEventOverLiveQuery(t *testing.T) {
	blockBaseFee := big.NewInt(166400000)
	b := newBaseFeeTestBackend(backendTestTxFeesQueryClient{baseFee: eipBaseFeeResponse(160000000)})
	blockRes := &cmrpctypes.ResultBlockResults{
		Height:              100,
		FinalizeBlockEvents: []abci.Event{backendTestBaseFeeEvent(blockBaseFee)},
	}

	baseFee, err := b.BaseFee(blockRes)

	require.NoError(t, err)
	require.Equal(t, 0, baseFee.Cmp(blockBaseFee))
}

func TestBaseFeeFallsBackToQueryWithoutBlockEvent(t *testing.T) {
	b := newBaseFeeTestBackend(backendTestTxFeesQueryClient{baseFee: eipBaseFeeResponse(160000000)})

	baseFee, err := b.BaseFee(&cmrpctypes.ResultBlockResults{Height: 100})

	require.NoError(t, err)
	require.Equal(t, 0, baseFee.Cmp(big.NewInt(160000000)))
}

func TestBaseFeeWithoutBlockEventReturnsQueryResult(t *testing.T) {
	queryErr := errors.New("unsupported query params")
	b := newBaseFeeTestBackend(backendTestTxFeesQueryClient{err: queryErr})

	baseFee, err := b.BaseFee(&cmrpctypes.ResultBlockResults{Height: 100})
	require.ErrorIs(t, err, queryErr)
	require.Nil(t, baseFee)

	b = newBaseFeeTestBackend(backendTestTxFeesQueryClient{
		baseFee: &txfeestypes.QueryEipBaseFeeResponse{},
	})
	baseFee, err = b.BaseFee(&cmrpctypes.ResultBlockResults{Height: 100})
	require.NoError(t, err)
	require.Nil(t, baseFee)
}
