package indexer

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

type stubSyncClient struct {
	status  *coretypes.ResultStatus
	blockFn func(context.Context, *int64) (*coretypes.ResultBlock, error)
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

func (s *stubSyncClient) BlockResults(context.Context, *int64) (*coretypes.ResultBlockResults, error) {
	return nil, errors.New("unexpected BlockResults call")
}

func (s *stubSyncClient) Validators(context.Context, *int64, *int, *int) (*coretypes.ResultValidators, error) {
	return nil, errors.New("unexpected Validators call")
}

type stubTxIndexer struct{}

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
