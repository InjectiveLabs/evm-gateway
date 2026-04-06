package types

import (
	"context"
	"errors"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	crypto "github.com/cometbft/cometbft/api/cometbft/crypto/v1"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/stretchr/testify/mock"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
)

func TestNewQueryClient(t *testing.T) {
	qc := NewQueryClient(client.Context{})
	if qc == nil || qc.ServiceClient == nil || qc.QueryClient == nil || qc.TxFeesQueryClient == nil {
		t.Fatalf("unexpected query client: %#v", qc)
	}
}

func TestQueryClientGetProof(t *testing.T) {
	qc := QueryClient{}
	if _, _, err := qc.GetProof(client.Context{}.WithHeight(2), "bank", []byte("k")); err == nil {
		t.Fatal("expected low-height proof query error")
	}

	mockClient := &rpcmocks.Client{}
	clientCtx := client.Context{}.
		WithClient(mockClient).
		WithHeight(9).
		WithCmdContext(context.Background())

	key := []byte("proof-key")
	proof := &crypto.ProofOps{Ops: []crypto.ProofOp{{Type: "iavl:v"}}}
	mockClient.
		On("ABCIQueryWithOptions", mock.Anything, "store/bank/key", cmbytes.HexBytes(key), rpcclient.ABCIQueryOptions{Height: 9, Prove: true}).
		Return(&coretypes.ResultABCIQuery{Response: abci.QueryResponse{Value: key, ProofOps: proof}}, nil)

	value, proofOps, err := qc.GetProof(clientCtx, "bank", key)
	if err != nil {
		t.Fatalf("GetProof returned error: %v", err)
	}
	if string(value) != string(key) || proofOps != proof {
		t.Fatalf("unexpected proof response: value=%x proof=%#v", value, proofOps)
	}

	mockClient = &rpcmocks.Client{}
	clientCtx = client.Context{}.WithClient(mockClient).WithHeight(10)
	mockClient.
		On("ABCIQueryWithOptions", mock.Anything, "store/bank/key", cmbytes.HexBytes(key), rpcclient.ABCIQueryOptions{Height: 10, Prove: true}).
		Return((*coretypes.ResultABCIQuery)(nil), errors.New("abci failed"))
	if _, _, err := qc.GetProof(clientCtx, "bank", key); err == nil {
		t.Fatal("expected ABCI query error")
	}
}
