package backend

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	txsigning "cosmossdk.io/x/tx/signing"
	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/metadata"
	protov2 "google.golang.org/protobuf/proto"
)

func TestTraceChainIDPrefersEthereumTxChainID(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	_, msg := mustSignedTraceMsg(
		t,
		&ethtypes.DynamicFeeTx{
			ChainID:   big.NewInt(1776),
			Nonce:     1,
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(2),
			Gas:       21000,
			To:        ptrAddress(common.HexToAddress("0x0000000000000000000000000000000000000001")),
			Value:     big.NewInt(0),
			Data:      []byte{0x1},
		},
		ethtypes.LatestSignerForChainID(big.NewInt(1776)),
		key,
	)

	if got := traceChainID(big.NewInt(1), msg); got != 1776 {
		t.Fatalf("unexpected trace chain id: got %d want 1776", got)
	}
}

func TestTraceChainIDFallsBackWhenTxChainIDUnavailable(t *testing.T) {
	if got := traceChainID(big.NewInt(1), &evmtypes.MsgEthereumTx{}); got != 1 {
		t.Fatalf("unexpected fallback chain id: got %d want 1", got)
	}
}

func TestTraceBlockContextHeightUsesResolvedBlock(t *testing.T) {
	cases := []struct {
		name        string
		height      rpctypes.BlockNumber
		blockHeight int64
		want        int64
	}{
		{
			name:        "latest uses resolved block height",
			height:      rpctypes.EthLatestBlockNumber,
			blockHeight: 171490488,
			want:        171490487,
		},
		{
			name:   "explicit block without resolved block",
			height: rpctypes.BlockNumber(100),
			want:   99,
		},
		{
			name:        "resolved block wins over requested sentinel",
			height:      rpctypes.EthPendingBlockNumber,
			blockHeight: 200,
			want:        199,
		},
		{
			name:        "genesis clamps to one",
			height:      rpctypes.EthEarliestBlockNumber,
			blockHeight: 1,
			want:        1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var block *cmrpctypes.ResultBlock
			if tc.blockHeight > 0 {
				block = &cmrpctypes.ResultBlock{Block: tmtypes.MakeBlock(tc.blockHeight, nil, nil, nil)}
			}
			if got := traceBlockContextHeight(tc.height, block); got != tc.want {
				t.Fatalf("traceBlockContextHeight() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAlignTraceBlockResultsWithVisibleTransactions(t *testing.T) {
	virtualBefore := common.HexToHash("0x01")
	ethereumHash := common.HexToHash("0x02")
	virtualAfter := common.HexToHash("0x03")
	ethereumResult := &rpctypes.TxTraceResult{
		TxHash: ethereumHash,
		Result: map[string]interface{}{"type": "CALL"},
	}

	aligned := alignTraceBlockResults(
		[]*rpctypes.TxTraceResult{ethereumResult},
		[]common.Hash{virtualBefore, ethereumHash, virtualAfter},
	)

	if len(aligned) != 3 {
		t.Fatalf("unexpected aligned trace result count: got %d want 3", len(aligned))
	}
	if aligned[0].TxHash != virtualBefore || aligned[1] != ethereumResult || aligned[2].TxHash != virtualAfter {
		t.Fatalf("trace results are not aligned to visible transaction order")
	}
	for _, index := range []int{0, 2} {
		result, ok := aligned[index].Result.(map[string]interface{})
		if !ok || result["type"] != 0 {
			t.Fatalf("expected an empty virtual trace at index %d, got %#v", index, aligned[index].Result)
		}
	}
}

func TestTraceBlockLatestSendsResolvedBlockContextHeight(t *testing.T) {
	const blockHeight int64 = 171490488
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, ethMsg := mustSignedTraceMsg(
		t,
		&ethtypes.DynamicFeeTx{
			ChainID:   big.NewInt(1776),
			Nonce:     1,
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(2),
			Gas:       21000,
			To:        ptrAddress(common.HexToAddress("0x0000000000000000000000000000000000000001")),
			Value:     big.NewInt(0),
		},
		ethtypes.LatestSignerForChainID(big.NewInt(1776)),
		key,
	)

	queryClient := &rpcmocks.EVMQueryClient{}
	queryClient.On(
		"TraceBlock",
		mock.MatchedBy(func(ctx context.Context) bool {
			md, ok := metadata.FromOutgoingContext(ctx)
			if !ok {
				return false
			}
			values := md.Get(grpctypes.GRPCBlockHeightHeader)
			return len(values) == 1 && values[0] == "171490487"
		}),
		mock.MatchedBy(func(req *evmtypes.QueryTraceBlockRequest) bool {
			return req.BlockNumber == blockHeight && len(req.Txs) == 1 && req.Txs[0] == ethMsg
		}),
	).Return(&evmtypes.QueryTraceBlockResponse{Data: []byte(`[{}]`)}, nil).Once()

	backend := &Backend{
		logger: backendTestLogger(),
		cfg:    appconfig.Config{},
		clientCtx: client.Context{}.WithTxConfig(backendTraceTestTxConfig{
			decoder: func([]byte) (sdk.Tx, error) {
				return backendTraceTestTx{msgs: []sdk.Msg{ethMsg}}, nil
			},
		}),
		queryClient: &rpctypes.QueryClient{QueryClient: queryClient},
		chainID:     big.NewInt(1776),
	}
	block := &cmrpctypes.ResultBlock{
		Block: tmtypes.MakeBlock(blockHeight, []tmtypes.Tx{[]byte("encoded-cosmos-tx")}, nil, nil),
	}

	got, err := backend.TraceBlock(rpctypes.EthLatestBlockNumber, nil, block)
	if err != nil {
		t.Fatalf("TraceBlock returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected trace result count: got %d want 1", len(got))
	}
	if got[0].TxHash != ethMsg.Hash() {
		t.Fatalf("unexpected trace transaction hash: got %s want %s", got[0].TxHash.Hex(), ethMsg.Hash().Hex())
	}
	queryClient.AssertExpectations(t)
}

func mustSignedTraceMsg(t *testing.T, txData ethtypes.TxData, signer ethtypes.Signer, key *ecdsa.PrivateKey) (*ethtypes.Transaction, *evmtypes.MsgEthereumTx) {
	t.Helper()
	tx := ethtypes.NewTx(txData)
	signed, err := ethtypes.SignTx(tx, signer, key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	msg := &evmtypes.MsgEthereumTx{}
	if err := msg.FromSignedEthereumTx(signed, signer); err != nil {
		t.Fatalf("FromSignedEthereumTx: %v", err)
	}
	return signed, msg
}

func ptrAddress(v common.Address) *common.Address {
	return &v
}

type backendTraceTestTxConfig struct {
	decoder sdk.TxDecoder
}

func (c backendTraceTestTxConfig) TxEncoder() sdk.TxEncoder     { return nil }
func (c backendTraceTestTxConfig) TxDecoder() sdk.TxDecoder     { return c.decoder }
func (c backendTraceTestTxConfig) TxJSONEncoder() sdk.TxEncoder { return nil }
func (c backendTraceTestTxConfig) TxJSONDecoder() sdk.TxDecoder { return nil }
func (c backendTraceTestTxConfig) MarshalSignatureJSON([]signingtypes.SignatureV2) ([]byte, error) {
	return nil, nil
}
func (c backendTraceTestTxConfig) UnmarshalSignatureJSON([]byte) ([]signingtypes.SignatureV2, error) {
	return nil, nil
}
func (c backendTraceTestTxConfig) NewTxBuilder() client.TxBuilder                 { return nil }
func (c backendTraceTestTxConfig) WrapTxBuilder(sdk.Tx) (client.TxBuilder, error) { return nil, nil }
func (c backendTraceTestTxConfig) SignModeHandler() *txsigning.HandlerMap         { return nil }
func (c backendTraceTestTxConfig) SigningContext() *txsigning.Context             { return nil }

type backendTraceTestTx struct {
	msgs []sdk.Msg
}

func (tx backendTraceTestTx) GetMsgs() []sdk.Msg {
	return tx.msgs
}

func (backendTraceTestTx) GetMsgsV2() ([]protov2.Message, error) {
	return nil, nil
}
