package types

import (
	"context"
	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	crypto "github.com/cometbft/cometbft/api/cometbft/crypto/v1"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/types/tx"
	"google.golang.org/grpc/metadata"
	"upd.dev/xlab/gotracer"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	txfeestypes "github.com/InjectiveLabs/sdk-go/chain/txfees/types"
)

// QueryClient defines a gRPC Client used for:
//   - Transaction simulation
//   - EVM module queries
//   - txfees module queries
type QueryClient struct {
	tx.ServiceClient
	evmtypes.QueryClient
	TxFeesQueryClient txfeestypes.QueryClient
}

var queryClientTraceTag = gotracer.NewTag("component", "rpc_query_client")

// NewQueryClient creates a new gRPC query client
func NewQueryClient(clientCtx client.Context) *QueryClient {
	return &QueryClient{
		ServiceClient:     tx.NewServiceClient(clientCtx),
		QueryClient:       evmtypes.NewQueryClient(clientCtx),
		TxFeesQueryClient: txfeestypes.NewQueryClient(clientCtx),
	}
}

// GetProof performs an ABCI query with the given key and returns a merkle proof. The desired
// tendermint height to perform the query should be set in the client context.
// The SDK passes this height to ABCI unchanged. Proof queries at height less
// than or equal to 2 are not supported.
// Issue: https://github.com/cosmos/cosmos-sdk/issues/6567
func (QueryClient) GetProof(clientCtx client.Context, storeKey string, key []byte) ([]byte, *crypto.ProofOps, error) {
	ctx := clientCtx.CmdContext
	if ctx != nil {
		defer gotracer.Trace(&ctx, queryClientTraceTag)()
	} else {
		ctx = context.Background()
		defer gotracer.Traceless(&ctx, queryClientTraceTag)()
	}

	height := clientCtx.Height
	// ABCI queries at height less than or equal to 2 are not supported.
	// Base app does not support queries for height less than or equal to 1.
	// Therefore, a query at height 2 would be equivalent to a query at height 3
	if height <= 2 {
		return nil, nil, fmt.Errorf("proof queries at height <= 2 are not supported")
	}

	abciReq := abci.QueryRequest{
		Path:   fmt.Sprintf("store/%s/key", storeKey),
		Data:   key,
		Height: height,
		Prove:  true,
	}
	// Cosmos client.QueryABCI replaces the operation context with Background.
	// Preserve the verified shard selection and deadline for Stitch reads.
	if md, _ := metadata.FromOutgoingContext(ctx); len(md.Get("x-stitch-backend")) != 0 {
		node, err := clientCtx.GetNode()
		if err != nil {
			return nil, nil, err
		}
		res, err := node.ABCIQueryWithOptions(ctx, abciReq.Path, abciReq.Data, cmrpcclient.ABCIQueryOptions{Height: height, Prove: true})
		if err != nil {
			return nil, nil, err
		}
		if res == nil || !res.Response.IsOK() || res.Response.Height != height || res.Response.ProofOps == nil {
			return nil, nil, fmt.Errorf("proof unavailable at resolved height %d", height)
		}
		return res.Response.Value, res.Response.ProofOps, nil
	}

	abciRes, err := clientCtx.QueryABCI(abciReq)
	if err != nil {
		return nil, nil, err
	}

	return abciRes.Value, abciRes.ProofOps, nil
}
