package types

import (
	"context"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	crypto "github.com/cometbft/cometbft/api/cometbft/crypto/v1"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/metadata"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
)

func TestStitchProofPreservesExactHeightAffinityAndDeadline(t *testing.T) {
	const height int64 = 127250000
	for _, store := range []string{"evm", "auth"} {
		t.Run(store, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-stitch-backend", "oldest"))
			deadline, _ := ctx.Deadline()
			node := rpcmocks.NewClient(t)
			proof := &crypto.ProofOps{Ops: []crypto.ProofOp{{Type: "ics23:iavl"}}}
			node.On("ABCIQueryWithOptions", mock.MatchedBy(func(actual context.Context) bool {
				md, _ := metadata.FromOutgoingContext(actual)
				d, ok := actual.Deadline()
				return ok && d.Equal(deadline) && len(md.Get("x-stitch-backend")) == 1 && md.Get("x-stitch-backend")[0] == "oldest"
			}), "store/"+store+"/key", cmbytes.HexBytes("key"), rpcclient.ABCIQueryOptions{Height: height, Prove: true}).Return(&coretypes.ResultABCIQuery{Response: abci.QueryResponse{Height: height, ProofOps: proof}}, nil).Once()
			_, got, err := (QueryClient{}).GetProof(client.Context{}.WithClient(node).WithHeight(height).WithCmdContext(ctx), store, []byte("key"))
			if err != nil || got != proof {
				t.Fatalf("proof=%v err=%v", got, err)
			}
		})
	}
}

func TestStitchProofRejectsUnverifiedResponses(t *testing.T) {
	const height int64 = 127250000
	for _, response := range []*coretypes.ResultABCIQuery{
		nil,
		{Response: abci.QueryResponse{Height: height}},
		{Response: abci.QueryResponse{Height: height - 1, ProofOps: &crypto.ProofOps{}}},
		{Response: abci.QueryResponse{Height: height, Code: 1, ProofOps: &crypto.ProofOps{}}},
	} {
		node := rpcmocks.NewClient(t)
		node.On("ABCIQueryWithOptions", mock.Anything, "store/auth/key", cmbytes.HexBytes("key"), rpcclient.ABCIQueryOptions{Height: height, Prove: true}).Return(response, nil).Once()
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-stitch-backend", "oldest"))
		_, _, err := (QueryClient{}).GetProof(client.Context{}.WithClient(node).WithHeight(height).WithCmdContext(ctx), "auth", []byte("key"))
		if err == nil {
			t.Fatalf("accepted unverified proof: %+v", response)
		}
	}
}
