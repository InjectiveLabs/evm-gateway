package backend

import (
	"context"
	"fmt"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func makeStorageKey(prefix []byte, key common.Hash) []byte {
	return append(append([]byte{}, prefix...), key.Bytes()...)
}

func TestStorageRangeAt(t *testing.T) {
	address := common.HexToAddress("0x1111111111111111111111111111111111111111")
	prefix := evmtypes.AddressStoragePrefix(address)

	key1 := common.HexToHash("0x01")
	key2 := common.HexToHash("0x02")
	key3 := common.HexToHash("0x03")
	val1 := common.HexToHash("0xaa")
	val2 := common.HexToHash("0xbb")
	val3 := common.HexToHash("0xcc")

	pairs := kvPairs{
		Pairs: []kvPair{
			{Key: makeStorageKey(prefix, key3), Value: val3.Bytes()},
			{Key: makeStorageKey(prefix, key1), Value: val1.Bytes()},
			{Key: makeStorageKey(prefix, key2), Value: val2.Bytes()},
		},
	}
	data, err := proto.Marshal(&pairs)
	require.NoError(t, err)

	bn := rpctypes.BlockNumber(1)
	blockArg := rpctypes.BlockNumberOrHash{BlockNumber: &bn}

	cometClient := mocks.NewClient(t)
	backend := &Backend{
		clientCtx:     client.Context{}.WithClient(cometClient),
		baseTraceTags: newBackendTraceTags(),
	}
	path := fmt.Sprintf("/store/%s/subspace", evmtypes.StoreKey)
	opts := cmrpcclient.ABCIQueryOptions{Height: bn.Int64(), Prove: false}
	cometClient.On(
		"ABCIQueryWithOptions",
		context.Background(),
		path,
		cmbytes.HexBytes(prefix),
		opts,
	).Return(&cmrpctypes.ResultABCIQuery{
		Response: abci.QueryResponse{
			Value:  data,
			Height: bn.Int64(),
		},
	}, nil)

	startKey := hexutil.Bytes(key2.Bytes())
	result, err := backend.StorageRangeAt(blockArg, 0, address, startKey, 1)
	require.NoError(t, err)
	require.Len(t, result.Storage, 1)

	entry, ok := result.Storage[key2]
	require.True(t, ok)
	require.Equal(t, &key2, entry.Key)
	require.Equal(t, val2, entry.Value)
	require.NotNil(t, result.NextKey)
	require.Equal(t, key3, *result.NextKey)
}
