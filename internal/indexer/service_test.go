package indexer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

func TestSyncerRunAcceptsRequestedEarliestWhenBlockIsAvailable(t *testing.T) {
	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   50,
			FetchJobs:  1,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   40,
					EarliestBlockHeight: 100,
				},
			},
			blockFn: func(_ context.Context, height *int64) (*coretypes.ResultBlock, error) {
				if height == nil || *height != 50 {
					t.Fatalf("expected validation probe for height 50, got %v", height)
				}
				return &coretypes.ResultBlock{Block: tmtypes.MakeBlock(*height, nil, nil, nil)}, nil
			},
		},
		nil,
		stubTxIndexer{},
		nil,
	)

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("expected syncer to accept requested earliest block, got error: %v", err)
	}
}

func TestSyncerRunUsesParsedLowestHeightWhenConfiguredEarliestIsUnavailable(t *testing.T) {
	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   50,
			FetchJobs:  1,
			AllowGaps:  true,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   55,
					EarliestBlockHeight: 1,
				},
			},
			blockFn: func(_ context.Context, height *int64) (*coretypes.ResultBlock, error) {
				if height == nil || *height != 50 {
					t.Fatalf("expected validation probe for height 50, got %v", height)
				}
				return nil, errors.New(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height 50 is not available, lowest height is 60"}}`)
			},
		},
		nil,
		stubTxIndexer{},
		nil,
	)

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("expected syncer to adjust to parsed earliest block, got error: %v", err)
	}
}

func TestSyncerRunRejectsUnavailableEarliestWithoutAllowGaps(t *testing.T) {
	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   50,
			FetchJobs:  1,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   55,
					EarliestBlockHeight: 1,
				},
			},
			blockFn: func(_ context.Context, _ *int64) (*coretypes.ResultBlock, error) {
				return nil, errors.New(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,"message":"Internal error","data":"height 50 is not available, lowest height is 60"}}`)
			},
		},
		nil,
		stubTxIndexer{},
		nil,
	)

	err := syncer.Run(context.Background())
	if err == nil {
		t.Fatal("expected unavailable earliest block to fail validation")
	}
	if err.Error() != "earliest block 50 before chain earliest 60" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncerResyncReindexesRequestedBlocks(t *testing.T) {
	indexer := &recordingTxIndexer{
		blockTxs: map[int64][]string{
			12: []string{"stale"},
		},
	}

	syncer := NewSyncer(
		config.Config{
			FetchJobs: 1,
		},
		testLogger(),
		&stubSyncClient{
			blockFn: func(_ context.Context, height *int64) (*coretypes.ResultBlock, error) {
				if height == nil {
					return nil, errors.New("nil height")
				}
				block, ok := testBlocks[*height]
				if !ok {
					return nil, errors.New("block not found")
				}
				return &coretypes.ResultBlock{Block: block}, nil
			},
			blockResultsFn: func(_ context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
				if height == nil {
					return nil, errors.New("nil height")
				}
				block, ok := testBlocks[*height]
				if !ok {
					return nil, errors.New("block results not found")
				}
				return &coretypes.ResultBlockResults{TxResults: make([]*abci.ExecTxResult, len(block.Txs))}, nil
			},
		},
		nil,
		indexer,
		nil,
	)

	stats, err := syncer.Resync(context.Background(), []BlockRange{
		{Start: 13, End: 13},
		{Start: 12, End: 13},
		{Start: 14, End: 14},
	})
	if err != nil {
		t.Fatalf("Resync returned error: %v", err)
	}

	if stats.BlocksSynced != 3 {
		t.Fatalf("unexpected blocks synced: got %d want 3", stats.BlocksSynced)
	}
	if stats.UniqueTxnsSeen != 3 {
		t.Fatalf("unexpected unique txs seen: got %d want 3", stats.UniqueTxnsSeen)
	}

	if got, want := indexer.order, []int64{12, 13, 14}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected indexed order: got %v want %v", got, want)
	}
	if got, want := indexer.blockTxs[12], []string{"tx-12a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected block 12 data: got %v want %v", got, want)
	}
	if got, want := indexer.blockTxs[13], []string{"tx-13a", "tx-13b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected block 13 data: got %v want %v", got, want)
	}
	if got, want := indexer.blockTxs[14], []string{"tx-14a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected block 14 data: got %v want %v", got, want)
	}
}

type stubSyncClient struct {
	status         *coretypes.ResultStatus
	blockFn        func(context.Context, *int64) (*coretypes.ResultBlock, error)
	blockResultsFn func(context.Context, *int64) (*coretypes.ResultBlockResults, error)
}

func (s *stubSyncClient) Status(_ context.Context) (*coretypes.ResultStatus, error) {
	return s.status, nil
}

func (s *stubSyncClient) Block(ctx context.Context, height *int64) (*coretypes.ResultBlock, error) {
	if s.blockFn == nil {
		return nil, errors.New("unexpected Block call")
	}
	return s.blockFn(ctx, height)
}

func (s *stubSyncClient) BlockResults(ctx context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	if s.blockResultsFn == nil {
		return nil, errors.New("unexpected BlockResults call")
	}
	return s.blockResultsFn(ctx, height)
}

func (s *stubSyncClient) Validators(context.Context, *int64, *int, *int) (*coretypes.ResultValidators, error) {
	return nil, errors.New("unexpected Validators call")
}

type stubTxIndexer struct{}

func (stubTxIndexer) WithContext(context.Context) TxIndexer                 { return stubTxIndexer{} }
func (stubTxIndexer) IndexBlock(*tmtypes.Block, []*abci.ExecTxResult) error { return nil }
func (stubTxIndexer) LastIndexedBlock() (int64, error)                      { return 0, nil }
func (stubTxIndexer) FirstIndexedBlock() (int64, error)                     { return 0, nil }
func (stubTxIndexer) GetByTxHash(common.Hash) (*chaintypes.TxResult, error) { return nil, nil }
func (stubTxIndexer) GetByBlockAndIndex(int64, int32) (*chaintypes.TxResult, error) {
	return nil, nil
}
func (stubTxIndexer) GetRPCTransactionByHash(common.Hash) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (stubTxIndexer) GetRPCTransactionByBlockAndIndex(int64, int32) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (stubTxIndexer) GetReceiptByTxHash(common.Hash) (map[string]interface{}, error) { return nil, nil }
func (stubTxIndexer) GetBlockMetaByHeight(int64) (*CachedBlockMeta, error)           { return nil, nil }
func (stubTxIndexer) GetLogsByBlockHeight(int64) ([][]*ethtypes.Log, error)          { return nil, nil }
func (stubTxIndexer) GetLogsByBlockHash(common.Hash) ([][]*ethtypes.Log, error)      { return nil, nil }
func (stubTxIndexer) IsBlockIndexed(int64) (bool, error)                             { return false, nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var testBlocks = map[int64]*tmtypes.Block{
	12: tmtypes.MakeBlock(12, []tmtypes.Tx{
		tmtypes.Tx("tx-12a"),
	}, nil, nil),
	13: tmtypes.MakeBlock(13, []tmtypes.Tx{
		tmtypes.Tx("tx-13a"),
		tmtypes.Tx("tx-13b"),
	}, nil, nil),
	14: tmtypes.MakeBlock(14, []tmtypes.Tx{
		tmtypes.Tx("tx-14a"),
	}, nil, nil),
}

type recordingTxIndexer struct {
	order    []int64
	blockTxs map[int64][]string
}

func (r *recordingTxIndexer) WithContext(context.Context) TxIndexer { return r }
func (r *recordingTxIndexer) IndexBlock(block *tmtypes.Block, _ []*abci.ExecTxResult) error {
	txNames := make([]string, 0, len(block.Txs))
	for _, tx := range block.Txs {
		txNames = append(txNames, string(tx))
	}
	r.order = append(r.order, block.Height)
	r.blockTxs[block.Height] = txNames
	return nil
}

func (r *recordingTxIndexer) IndexBlockWithStats(block *tmtypes.Block, txResults []*abci.ExecTxResult) (BlockIndexStats, error) {
	if err := r.IndexBlock(block, txResults); err != nil {
		return BlockIndexStats{}, err
	}

	switch block.Height {
	case 12:
		return BlockIndexStats{IndexedEthTxs: 1}, nil
	case 13:
		return BlockIndexStats{IndexedEthTxs: 2}, nil
	case 14:
		return BlockIndexStats{IndexedEthTxs: 0}, nil
	default:
		return BlockIndexStats{}, nil
	}
}

func (r *recordingTxIndexer) LastIndexedBlock() (int64, error)  { return 0, nil }
func (r *recordingTxIndexer) FirstIndexedBlock() (int64, error) { return 0, nil }
func (r *recordingTxIndexer) GetByTxHash(common.Hash) (*chaintypes.TxResult, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetByBlockAndIndex(int64, int32) (*chaintypes.TxResult, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetRPCTransactionByHash(common.Hash) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetRPCTransactionByBlockAndIndex(int64, int32) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetReceiptByTxHash(common.Hash) (map[string]interface{}, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetBlockMetaByHeight(int64) (*CachedBlockMeta, error) { return nil, nil }
func (r *recordingTxIndexer) GetLogsByBlockHeight(int64) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetLogsByBlockHash(common.Hash) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (r *recordingTxIndexer) IsBlockIndexed(int64) (bool, error) { return false, nil }
