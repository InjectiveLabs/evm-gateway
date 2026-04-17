package indexer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"

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

func TestSyncerResyncDeletesFailedBlockAndReturnsError(t *testing.T) {
	indexer := &faultyTxIndexer{
		blockTxs:    map[int64][]string{12: []string{"stale"}},
		failHeights: map[int64]error{12: newBlockParseError(errors.New("bad event"), "block 12 txIndex 0: failed to parse tx result")},
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

	_, err := syncer.Resync(context.Background(), []BlockRange{{Start: 12, End: 12}})
	if err == nil {
		t.Fatal("expected resync to return an error")
	}
	if !errors.Is(err, ErrBlockParse) {
		t.Fatalf("expected parse error, got %v", err)
	}
	if _, ok := indexer.blockTxs[12]; ok {
		t.Fatalf("expected failed block to be deleted, got %v", indexer.blockTxs[12])
	}
	if got, want := indexer.deleted, []int64{12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunContinuesToNextGapAfterParseErrorWithoutAllowGaps(t *testing.T) {
	db := dbm.NewMemDB()
	if err := db.Set(BlockMetaKey(3), mustJSON(CachedBlockMeta{Height: 3, Hash: common.HexToHash("0x03").Hex()})); err != nil {
		t.Fatalf("set block meta: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	indexer := &faultyTxIndexer{
		blockTxs:    make(map[int64][]string),
		failHeights: map[int64]error{2: newBlockParseError(errors.New("bad event"), "block 2 txIndex 0: failed to parse tx result")},
		onSuccess: func(height int64) {
			if height == 4 {
				cancel()
			}
		},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   1,
			FetchJobs:  1,
			AllowGaps:  false,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   4,
					EarliestBlockHeight: 1,
				},
			},
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
		db,
		indexer,
		nil,
	)

	err := syncer.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected run to stop on context cancel after continuing gaps, got %v", err)
	}
	if got, want := indexer.successes, []int64{1, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected indexed heights: got %v want %v", got, want)
	}
	if got, want := indexer.deleted, []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunSkipsParseErrorWhenAllowGapsEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	indexer := &faultyTxIndexer{
		blockTxs:    make(map[int64][]string),
		failHeights: map[int64]error{1: newBlockParseError(errors.New("bad event"), "block 1 txIndex 0: failed to parse tx result")},
		onSuccess: func(height int64) {
			if height == 2 {
				cancel()
			}
		},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   1,
			FetchJobs:  1,
			AllowGaps:  true,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   2,
					EarliestBlockHeight: 1,
				},
			},
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
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	err := syncer.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected run to stop on context cancel after skipping parse error, got %v", err)
	}
	if got, want := indexer.successes, []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected indexed heights: got %v want %v", got, want)
	}
	if got, want := indexer.deleted, []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunStopsForwardSyncOnParseErrorAndReturnsNil(t *testing.T) {
	db := dbm.NewMemDB()
	if err := db.Set(BlockMetaKey(1), mustJSON(CachedBlockMeta{Height: 1, Hash: common.HexToHash("0x01").Hex()})); err != nil {
		t.Fatalf("set block meta: %v", err)
	}

	indexer := &faultyTxIndexer{
		blockTxs:    make(map[int64][]string),
		failHeights: map[int64]error{2: newBlockParseError(errors.New("bad event"), "block 2 txIndex 0: failed to parse tx result")},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync: true,
			Earliest:   1,
			FetchJobs:  1,
			AllowGaps:  false,
		},
		testLogger(),
		&stubSyncClient{
			status: &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{
					LatestBlockHeight:   1,
					EarliestBlockHeight: 1,
				},
			},
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
		db,
		indexer,
		nil,
	)

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("expected forward sync parse error to stop sync without failing app, got %v", err)
	}
	if got, want := indexer.deleted, []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunParallelStartsTipBeforeGapCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gapStarted := make(chan struct{})
	releaseGap := make(chan struct{})
	tipIndexed := make(chan struct{})
	var gapStartedOnce sync.Once
	var releaseGapOnce sync.Once
	var tipIndexedOnce sync.Once
	defer releaseGapOnce.Do(func() { close(releaseGap) })

	indexer := &concurrentTxIndexer{
		blockTxs: make(map[int64][]string),
		onIndex: func(height int64) {
			switch height {
			case 1:
				gapStartedOnce.Do(func() { close(gapStarted) })
				<-releaseGap
			case 4:
				tipIndexedOnce.Do(func() { close(tipIndexed) })
			}
		},
	}
	client := testSyncClientWithHead(3, nil)

	syncer := NewSyncer(
		config.Config{
			EnableSync:             true,
			Earliest:               1,
			FetchJobs:              1,
			AllowGaps:              false,
			ParallelSyncTipAndGaps: true,
		},
		testLogger(),
		client,
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	done := make(chan error, 1)
	go func() {
		done <- syncer.Run(ctx)
	}()

	waitForSignal(t, gapStarted, time.Second, "gap sync to start")
	waitForSignal(t, tipIndexed, time.Second, "tip sync to index startup head + 1 while gap is still blocked")

	releaseGapOnce.Do(func() { close(releaseGap) })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected syncer to stop on context cancel, got %v", err)
	}
}

func TestSyncerRunSerialDoesNotStartTipUntilGapsComplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gapStarted := make(chan struct{})
	releaseGap := make(chan struct{})
	tipIndexed := make(chan struct{})
	var gapStartedOnce sync.Once
	var releaseGapOnce sync.Once
	var tipIndexedOnce sync.Once
	defer releaseGapOnce.Do(func() { close(releaseGap) })

	indexer := &concurrentTxIndexer{
		blockTxs: make(map[int64][]string),
		onIndex: func(height int64) {
			switch height {
			case 1:
				gapStartedOnce.Do(func() { close(gapStarted) })
				<-releaseGap
			case 4:
				tipIndexedOnce.Do(func() { close(tipIndexed) })
			}
		},
	}
	client := testSyncClientWithHead(3, nil)

	syncer := NewSyncer(
		config.Config{
			EnableSync:             true,
			Earliest:               1,
			FetchJobs:              1,
			AllowGaps:              false,
			ParallelSyncTipAndGaps: false,
		},
		testLogger(),
		client,
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	done := make(chan error, 1)
	go func() {
		done <- syncer.Run(ctx)
	}()

	waitForSignal(t, gapStarted, time.Second, "gap sync to start")
	select {
	case <-tipIndexed:
		t.Fatal("tip sync indexed a block before serial gap sync was released")
	case <-time.After(200 * time.Millisecond):
	}

	releaseGapOnce.Do(func() { close(releaseGap) })
	waitForSignal(t, tipIndexed, time.Second, "tip sync to index after gaps complete")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected syncer to stop on context cancel, got %v", err)
	}

	if got, want := indexer.SuccessesPrefix(3), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected gaps to be indexed before tip sync starts, got prefix %v want %v", got, want)
	}
}

func TestSyncerRunParallelGapErrorDoesNotStopTipSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	indexer := &concurrentTxIndexer{
		blockTxs:    make(map[int64][]string),
		failHeights: map[int64]error{1: errors.New("gap failed")},
		onIndex: func(height int64) {
			if height == 2 {
				cancel()
			}
		},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync:             true,
			Earliest:               1,
			FetchJobs:              1,
			AllowGaps:              false,
			ParallelSyncTipAndGaps: true,
		},
		testLogger(),
		testSyncClientWithHead(1, nil),
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	if err := syncer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected syncer to stop on context cancel after tip continued, got %v", err)
	}
	if !indexer.HasSuccess(2) {
		t.Fatalf("expected tip block 2 to be indexed after gap failure, got %v", indexer.Successes())
	}
	if got, want := indexer.Deleted(), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunParallelTipErrorDoesNotStopGapSync(t *testing.T) {
	indexer := &concurrentTxIndexer{
		blockTxs:    make(map[int64][]string),
		failHeights: map[int64]error{4: errors.New("tip failed")},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync:             true,
			Earliest:               1,
			FetchJobs:              1,
			AllowGaps:              false,
			ParallelSyncTipAndGaps: true,
		},
		testLogger(),
		testSyncClientWithHead(3, nil),
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("expected tip failure to stop only the tip queue, got %v", err)
	}
	if got, want := indexer.Successes(), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected indexed heights after tip failure: got %v want %v", got, want)
	}
	if got, want := indexer.Deleted(), []int64{4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
	}
}

func TestSyncerRunParallelReturnsNilWhenTipAndGapQueuesError(t *testing.T) {
	indexer := &concurrentTxIndexer{
		blockTxs: make(map[int64][]string),
		failHeights: map[int64]error{
			1: errors.New("gap failed"),
			2: errors.New("tip failed"),
		},
	}

	syncer := NewSyncer(
		config.Config{
			EnableSync:             true,
			Earliest:               1,
			FetchJobs:              1,
			AllowGaps:              false,
			ParallelSyncTipAndGaps: true,
		},
		testLogger(),
		testSyncClientWithHead(1, nil),
		dbm.NewMemDB(),
		indexer,
		nil,
	)

	if err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("expected both queue failures to leave gateway running, got %v", err)
	}
	if got, want := indexer.Deleted(), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deleted heights: got %v want %v", got, want)
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
func (stubTxIndexer) DeleteBlock(int64) error                               { return nil }
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
func (stubTxIndexer) GetRPCTransactionHashesByBlockHeight(int64) ([]common.Hash, error) {
	return nil, nil
}
func (stubTxIndexer) GetReceiptByTxHash(common.Hash) (map[string]interface{}, error) { return nil, nil }
func (stubTxIndexer) GetBlockMetaByHeight(int64) (*CachedBlockMeta, error)           { return nil, nil }
func (stubTxIndexer) GetBlockMetaByHash(common.Hash) (*CachedBlockMeta, error)       { return nil, nil }
func (stubTxIndexer) GetLogsByBlockHeight(int64) ([][]*ethtypes.Log, error)          { return nil, nil }
func (stubTxIndexer) GetLogsByBlockHash(common.Hash) ([][]*ethtypes.Log, error)      { return nil, nil }
func (stubTxIndexer) SetTraceTransaction(common.Hash, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (stubTxIndexer) GetTraceTransaction(common.Hash, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (stubTxIndexer) SetTraceBlockByHeight(int64, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (stubTxIndexer) GetTraceBlockByHeight(int64, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (stubTxIndexer) IsBlockIndexed(int64) (bool, error) { return false, nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testSyncClientWithHead(head int64, observeHeight func(int64)) *stubSyncClient {
	return &stubSyncClient{
		status: &coretypes.ResultStatus{
			SyncInfo: coretypes.SyncInfo{
				LatestBlockHeight:   head,
				EarliestBlockHeight: 1,
			},
		},
		blockFn: func(_ context.Context, height *int64) (*coretypes.ResultBlock, error) {
			if height == nil {
				return nil, errors.New("nil height")
			}
			if observeHeight != nil {
				observeHeight(*height)
			}
			return &coretypes.ResultBlock{Block: testBlock(*height)}, nil
		},
		blockResultsFn: func(_ context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
			if height == nil {
				return nil, errors.New("nil height")
			}
			if observeHeight != nil {
				observeHeight(*height)
			}
			block := testBlock(*height)
			return &coretypes.ResultBlockResults{TxResults: make([]*abci.ExecTxResult, len(block.Txs))}, nil
		},
	}
}

func testBlock(height int64) *tmtypes.Block {
	if block, ok := testBlocks[height]; ok {
		return block
	}
	return tmtypes.MakeBlock(height, nil, nil, nil)
}

func waitForSignal(t *testing.T, ch <-chan struct{}, timeout time.Duration, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

var testBlocks = map[int64]*tmtypes.Block{
	1: tmtypes.MakeBlock(1, []tmtypes.Tx{
		tmtypes.Tx("tx-1a"),
	}, nil, nil),
	2: tmtypes.MakeBlock(2, []tmtypes.Tx{
		tmtypes.Tx("tx-2a"),
	}, nil, nil),
	3: tmtypes.MakeBlock(3, []tmtypes.Tx{
		tmtypes.Tx("tx-3a"),
	}, nil, nil),
	4: tmtypes.MakeBlock(4, []tmtypes.Tx{
		tmtypes.Tx("tx-4a"),
	}, nil, nil),
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

func (r *recordingTxIndexer) DeleteBlock(height int64) error {
	delete(r.blockTxs, height)
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
func (r *recordingTxIndexer) GetRPCTransactionHashesByBlockHeight(int64) ([]common.Hash, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetReceiptByTxHash(common.Hash) (map[string]interface{}, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetBlockMetaByHeight(int64) (*CachedBlockMeta, error) { return nil, nil }
func (r *recordingTxIndexer) GetBlockMetaByHash(common.Hash) (*CachedBlockMeta, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetLogsByBlockHeight(int64) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (r *recordingTxIndexer) GetLogsByBlockHash(common.Hash) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (r *recordingTxIndexer) SetTraceTransaction(common.Hash, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (r *recordingTxIndexer) GetTraceTransaction(common.Hash, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (r *recordingTxIndexer) SetTraceBlockByHeight(int64, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (r *recordingTxIndexer) GetTraceBlockByHeight(int64, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (r *recordingTxIndexer) IsBlockIndexed(int64) (bool, error) { return false, nil }

type concurrentTxIndexer struct {
	stubTxIndexer

	mu          sync.Mutex
	successes   []int64
	deleted     []int64
	blockTxs    map[int64][]string
	failHeights map[int64]error
	onIndex     func(int64)
}

func (c *concurrentTxIndexer) WithContext(context.Context) TxIndexer { return c }

func (c *concurrentTxIndexer) IndexBlock(block *tmtypes.Block, _ []*abci.ExecTxResult) error {
	if c.onIndex != nil {
		c.onIndex(block.Height)
	}
	if err, ok := c.failHeights[block.Height]; ok {
		return err
	}

	txNames := make([]string, 0, len(block.Txs))
	for _, tx := range block.Txs {
		txNames = append(txNames, string(tx))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.successes = append(c.successes, block.Height)
	if c.blockTxs != nil {
		c.blockTxs[block.Height] = txNames
	}
	return nil
}

func (c *concurrentTxIndexer) DeleteBlock(height int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, height)
	if c.blockTxs != nil {
		delete(c.blockTxs, height)
	}
	return nil
}

func (c *concurrentTxIndexer) Successes() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.successes...)
}

func (c *concurrentTxIndexer) SuccessesPrefix(n int) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.successes) < n {
		n = len(c.successes)
	}
	return append([]int64(nil), c.successes[:n]...)
}

func (c *concurrentTxIndexer) HasSuccess(height int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, success := range c.successes {
		if success == height {
			return true
		}
	}
	return false
}

func (c *concurrentTxIndexer) Deleted() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	deleted := append([]int64(nil), c.deleted...)
	sort.Slice(deleted, func(i, j int) bool {
		return deleted[i] < deleted[j]
	})
	return deleted
}

type faultyTxIndexer struct {
	successes   []int64
	deleted     []int64
	blockTxs    map[int64][]string
	failHeights map[int64]error
	onSuccess   func(int64)
}

func (f *faultyTxIndexer) WithContext(context.Context) TxIndexer { return f }

func (f *faultyTxIndexer) IndexBlock(block *tmtypes.Block, _ []*abci.ExecTxResult) error {
	if err, ok := f.failHeights[block.Height]; ok {
		return err
	}

	txNames := make([]string, 0, len(block.Txs))
	for _, tx := range block.Txs {
		txNames = append(txNames, string(tx))
	}
	f.successes = append(f.successes, block.Height)
	f.blockTxs[block.Height] = txNames
	if f.onSuccess != nil {
		f.onSuccess(block.Height)
	}
	return nil
}

func (f *faultyTxIndexer) DeleteBlock(height int64) error {
	f.deleted = append(f.deleted, height)
	delete(f.blockTxs, height)
	return nil
}

func (f *faultyTxIndexer) LastIndexedBlock() (int64, error)  { return 0, nil }
func (f *faultyTxIndexer) FirstIndexedBlock() (int64, error) { return 0, nil }
func (f *faultyTxIndexer) GetByTxHash(common.Hash) (*chaintypes.TxResult, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetByBlockAndIndex(int64, int32) (*chaintypes.TxResult, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetRPCTransactionByHash(common.Hash) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetRPCTransactionByBlockAndIndex(int64, int32) (*rpctypes.RPCTransaction, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetRPCTransactionHashesByBlockHeight(int64) ([]common.Hash, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetReceiptByTxHash(common.Hash) (map[string]interface{}, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetBlockMetaByHeight(int64) (*CachedBlockMeta, error) { return nil, nil }
func (f *faultyTxIndexer) GetBlockMetaByHash(common.Hash) (*CachedBlockMeta, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetLogsByBlockHeight(int64) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (f *faultyTxIndexer) GetLogsByBlockHash(common.Hash) ([][]*ethtypes.Log, error) {
	return nil, nil
}
func (f *faultyTxIndexer) SetTraceTransaction(common.Hash, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (f *faultyTxIndexer) GetTraceTransaction(common.Hash, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (f *faultyTxIndexer) SetTraceBlockByHeight(int64, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}
func (f *faultyTxIndexer) GetTraceBlockByHeight(int64, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, nil
}
func (f *faultyTxIndexer) IsBlockIndexed(int64) (bool, error) { return false, nil }
