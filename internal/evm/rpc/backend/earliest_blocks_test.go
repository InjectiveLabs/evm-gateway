package backend

import (
	"context"
	"errors"
	"math/big"
	"testing"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type earliestCometClient struct{ *rpcmocks.Client }

func (*earliestCometClient) SetLogger(cmtlog.Logger) {}

func TestStitchEarliestTraceUsesBlockHAndStateHMinusOne(t *testing.T) {
	peer := &earliestPeer{headers: goodEarliestHeaders()}
	b := earliestBackend(t, startEarliestPeer(t, peer), true)
	fixture := newDynamicFeeReceiptFixture(t)
	fixture.block.Height = fixtureEarliestHeight
	b.clientCtx = b.clientCtx.WithTxConfig(backendReceiptTestTxConfig{tx: fixture.decodedTx})
	b.chainID = fixture.chainID
	node := rpcmocks.NewClient(t)
	b.clientCtx = b.clientCtx.WithClient(node)
	height := fixtureEarliestHeight
	node.On("Block", mock.MatchedBy(func(ctx context.Context) bool {
		md, _ := metadata.FromOutgoingContext(ctx)
		return firstMD(md, "x-stitch-backend") == "oldest" && len(md.Get("stitch")) == 0
	}), &height).Return(&cmrpctypes.ResultBlock{Block: fixture.block}, nil).Once()
	traces, err := b.TraceBlock(rpctypes.EthEarliestBlockNumber, nil, nil)
	if err != nil || len(traces) != 1 {
		t.Fatalf("trace=%v err=%v", traces, err)
	}
	calls := peer.snapshot()
	if len(calls) != 2 || firstMD(calls[0].md, "x-stitch-earliest-capability") != "trace" {
		t.Fatalf("wrong trace discovery: %+v", calls)
	}
	call := calls[1]
	if call.method != "TraceBlock" || call.requestHeight != height || firstMD(call.md, "x-cosmos-block-height") != "127000122" || firstMD(call.md, "x-stitch-backend") != "oldest" || firstMD(call.md, "stitch") != "" {
		t.Fatalf("trace mixed snapshot or shard: %+v", call)
	}
}

func TestStitchEarliestFeeHistoryClampsToOneActualBlock(t *testing.T) {
	peer := &earliestPeer{headers: goodEarliestHeaders()}
	b := earliestBackend(t, startEarliestPeer(t, peer), true)
	b.cfg.JSONRPC.FeeHistoryCap = 100
	node := rpcmocks.NewClient(t)
	b.clientCtx = b.clientCtx.WithClient(&earliestCometClient{Client: node})
	height := fixtureEarliestHeight
	selectedContext := mock.MatchedBy(func(ctx context.Context) bool {
		md, _ := metadata.FromOutgoingContext(ctx)
		return firstMD(md, "x-stitch-backend") == "oldest" && len(md.Get("stitch")) == 0
	})
	block := &cmrpctypes.ResultBlock{Block: tmtypes.MakeBlock(height, nil, nil, nil)}
	results := &cmrpctypes.ResultBlockResults{Height: height}
	node.On("Block", selectedContext, &height).Return(block, nil).Twice()
	node.On("BlockResults", selectedContext, &height).Return(results, nil).Twice()
	node.On("ConsensusParams", selectedContext, &height).Return(&cmrpctypes.ResultConsensusParams{ConsensusParams: *tmtypes.DefaultConsensusParams()}, nil).Once()
	b.processBlocker = func(block *cmrpctypes.ResultBlock, _ map[string]interface{}, _ []float64, results *cmrpctypes.ResultBlockResults, fee *rpctypes.OneFeeHistory) error {
		if block.Block.Height != height || results.Height != height {
			t.Error("fee history mixed snapshots")
		}
		fee.BaseFee, fee.NextBaseFee = big.NewInt(10), big.NewInt(11)
		return nil
	}
	result, err := b.FeeHistory(math.HexOrDecimal64(5), rpc.EarliestBlockNumber, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OldestBlock.ToInt().Int64() != height || len(result.BaseFee) != 2 || len(result.GasUsedRatio) != 1 {
		t.Fatalf("fee history included pre-EVM blocks: %+v", result)
	}
	params := 0
	for _, call := range peer.snapshot() {
		if call.method != "Params" {
			continue
		}
		params++
		if params == 1 {
			if firstMD(call.md, "stitch") != "earliest" || firstMD(call.md, "x-stitch-earliest-capability") != "range" {
				t.Errorf("wrong fee discovery: %+v", call)
			}
		} else if firstMD(call.md, "stitch") != "" || firstMD(call.md, "x-stitch-backend") != "" || firstMD(call.md, "x-cosmos-block-height") != "" {
			t.Errorf("head query inherited earliest markers: %+v", call)
		}
	}
	if params != 2 {
		t.Fatalf("wanted one discovery and one latest head read, got %d", params)
	}
}

func TestStitchEarliestBlockMethodsPropagateDiscoveryFailures(t *testing.T) {
	for _, method := range []string{"block", "tx count", "transaction", "header", "proof", "balance", "fee history"} {
		t.Run(method, func(t *testing.T) {
			peer := &earliestPeer{discoveryErr: status.Error(codes.Unavailable, "incomplete earliest search")}
			b := earliestBackend(t, startEarliestPeer(t, peer), true)
			h := rpctypes.EthEarliestBlockNumber
			selector := rpctypes.BlockNumberOrHash{BlockNumber: &h}
			var err error
			wantCapability := "block"
			switch method {
			case "block":
				_, err = b.GetBlockByNumber(h, false)
			case "tx count":
				_, err = b.GetBlockTransactionCountByNumber(h)
			case "transaction":
				_, err = b.GetTransactionByBlockNumberAndIndex(h, 0)
			case "header":
				_, err = b.HeaderByNumber(h)
			case "proof":
				_, err = b.GetProof(common.Address{}, nil, selector)
				wantCapability = "proof"
			case "balance":
				_, err = b.GetBalance(common.Address{}, selector)
				wantCapability = "execution"
			case "fee history":
				_, err = b.FeeHistory(math.HexOrDecimal64(5), rpc.EarliestBlockNumber, nil)
				wantCapability = "range"
			}
			if err == nil {
				t.Fatalf("%s masked discovery failure", method)
			}
			calls := peer.snapshot()
			if len(calls) != 1 || calls[0].method != "Params" || firstMD(calls[0].md, "x-stitch-earliest-capability") != wantCapability {
				t.Fatalf("wrong %s discovery: %+v", method, calls)
			}
		})
	}
}

func TestStitchEarliestBlockReadsKeepAffinityAndPropagateFailures(t *testing.T) {
	for _, method := range []string{"block", "tx count"} {
		for _, scenario := range []string{"block unavailable", "missing block", "results unavailable"} {
			if method == "tx count" && scenario == "results unavailable" {
				continue
			}
			t.Run(method+"/"+scenario, func(t *testing.T) {
				peer := &earliestPeer{headers: goodEarliestHeaders()}
				b := earliestBackend(t, startEarliestPeer(t, peer), true)
				node := rpcmocks.NewClient(t)
				b.clientCtx = b.clientCtx.WithClient(node)
				height := fixtureEarliestHeight
				selectedContext := mock.MatchedBy(func(ctx context.Context) bool {
					md, _ := metadata.FromOutgoingContext(ctx)
					return firstMD(md, "x-stitch-backend") == "oldest" && len(md.Get("stitch")) == 0
				})
				block := &cmrpctypes.ResultBlock{Block: tmtypes.MakeBlock(height, nil, nil, nil)}
				switch scenario {
				case "block unavailable":
					node.On("Block", selectedContext, &height).Return((*cmrpctypes.ResultBlock)(nil), errors.New("shard lost block")).Once()
				case "missing block":
					node.On("Block", selectedContext, &height).Return(&cmrpctypes.ResultBlock{}, nil).Once()
				case "results unavailable":
					node.On("Block", selectedContext, &height).Return(block, nil).Once()
					node.On("BlockResults", selectedContext, &height).Return((*cmrpctypes.ResultBlockResults)(nil), errors.New("shard lost results")).Once()
				}
				var err error
				if method == "block" {
					_, err = b.GetBlockByNumber(rpctypes.EthEarliestBlockNumber, false)
				} else {
					_, err = b.GetBlockTransactionCountByNumber(rpctypes.EthEarliestBlockNumber)
				}
				if err == nil {
					t.Fatalf("%s masked %s after discovery", method, scenario)
				}
			})
		}
	}
}

func TestStitchExplicitMissingBlockRetainsNullBehavior(t *testing.T) {
	for _, method := range []string{"block", "tx count"} {
		t.Run(method, func(t *testing.T) {
			peer := &earliestPeer{}
			b := earliestBackend(t, startEarliestPeer(t, peer), true)
			node := rpcmocks.NewClient(t)
			b.clientCtx = b.clientCtx.WithClient(node)
			height := fixtureEarliestHeight
			node.On("Block", mock.Anything, &height).Return((*cmrpctypes.ResultBlock)(nil), errors.New("block unavailable")).Once()
			if method == "block" {
				got, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), false)
				if got != nil || err != nil {
					t.Fatalf("ordinary missing block changed: %v %v", got, err)
				}
			} else {
				got, err := b.GetBlockTransactionCountByNumber(rpctypes.BlockNumber(height))
				if got != nil || err != nil {
					t.Fatalf("ordinary missing block count changed: %v %v", got, err)
				}
			}
			if len(peer.snapshot()) != 0 {
				t.Fatal("numeric request did discovery")
			}
		})
	}
}

func TestStitchEarliestDerivedContextsClearInheritedRouting(t *testing.T) {
	peer := &earliestPeer{headers: goodEarliestHeaders()}
	b := earliestBackend(t, startEarliestPeer(t, peer), true)
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-stitch-backend", "stale", "stitch", "earliest", "x-stitch-earliest-capability", "trace", "x-cosmos-block-height", "3", "authorization", "fixture-token"))
	op := b.WithContext(ctx).(*Backend)
	if _, err := op.ResolveEarliestBlockNumber(rpctypes.EthEarliestBlockNumber, "state"); err != nil {
		t.Fatal(err)
	}
	if calls := peer.snapshot(); len(calls) != 1 || len(calls[0].md.Get("x-stitch-backend")) != 0 {
		t.Fatalf("stale affinity on discovery: %+v", calls)
	}
	for _, height := range []int64{0, fixtureEarliestHeight, fixtureEarliestHeight + 1} {
		md, _ := metadata.FromOutgoingContext(op.contextWithHeight(height))
		if len(md.Get("stitch")) != 0 || len(md.Get("x-stitch-earliest-capability")) != 0 {
			t.Fatalf("discovery marker leaked: %v", md)
		}
		wantBackend := ""
		if height == fixtureEarliestHeight {
			wantBackend = "oldest"
		}
		if firstMD(md, "x-stitch-backend") != wantBackend {
			t.Errorf("height%d wrong affinity: %v", height, md)
		}
		if firstMD(md, "authorization") != "fixture-token" {
			t.Fatal("unrelated metadata dropped")
		}
	}
}
