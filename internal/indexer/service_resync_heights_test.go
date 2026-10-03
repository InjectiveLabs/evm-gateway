package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func fixtureSyncClient(t *testing.T) *stubSyncClient {
	t.Helper()
	return &stubSyncClient{
		blockFn: func(_ context.Context, height *int64) (*coretypes.ResultBlock, error) {
			return mainnetfx.ResultBlock(*height)
		},
		blockResultsFn: func(_ context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
			return mainnetfx.BlockResults(*height)
		},
	}
}

// TestResyncHeightsRepairsLegacyState resyncs the scattered fixture heights
// (unordered, with duplicates) on top of the pre-fix KV state with parallel
// fetches; the result must equal a fresh index.
func TestResyncHeightsRepairsLegacyState(t *testing.T) {
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))
	syncer := NewSyncer(config.Config{FetchJobs: 4}, testLogger(), fixtureSyncClient(t), db, kv, nil)

	heights := []int64{
		mainnetfx.HeightEx2Included,
		mainnetfx.HeightEx1Failed,
		mainnetfx.HeightEx2Failed,
		mainnetfx.HeightEx1Included,
		mainnetfx.HeightEx1Failed,
	}
	stats, err := syncer.ResyncHeights(context.Background(), heights)
	if err != nil {
		t.Fatalf("ResyncHeights: %v", err)
	}
	if stats.BlocksSynced != 4 {
		t.Fatalf("unexpected blocks synced: %d", stats.BlocksSynced)
	}
	var visible int64
	for _, height := range mainnetfx.Heights {
		visible += int64(len(mainnetfx.VisibleTxs[height]))
	}
	if stats.UniqueTxnsSeen != visible {
		t.Fatalf("unexpected txs seen: got %d want %d", stats.UniqueTxnsSeen, visible)
	}

	assertSameDB(t, dumpDB(t, db, KeyPrefixMigration), dumpDB(t, freshFixtureDB(t, mainnetfx.Heights...), KeyPrefixMigration))
}

func TestResyncHeightsErrors(t *testing.T) {
	t.Run("no indexer", func(t *testing.T) {
		syncer := NewSyncer(config.Config{FetchJobs: 2}, testLogger(), fixtureSyncClient(t), nil, nil, nil)
		if _, err := syncer.ResyncHeights(context.Background(), []int64{1}); err == nil || !strings.Contains(err.Error(), "tx indexer not configured") {
			t.Fatalf("expected missing indexer error, got %v", err)
		}
	})

	t.Run("no heights", func(t *testing.T) {
		syncer := NewSyncer(config.Config{FetchJobs: 2}, testLogger(), fixtureSyncClient(t), nil, &stubTxIndexer{}, nil)
		if _, err := syncer.ResyncHeights(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no resync heights") {
			t.Fatalf("expected empty heights error, got %v", err)
		}
	})

	t.Run("index failure cleans up the block", func(t *testing.T) {
		indexer := &faultyTxIndexer{
			blockTxs:    map[int64][]string{},
			failHeights: map[int64]error{mainnetfx.HeightEx1Included: errors.New("bad block")},
		}
		syncer := NewSyncer(config.Config{FetchJobs: 2}, testLogger(), fixtureSyncClient(t), nil, indexer, nil)
		_, err := syncer.ResyncHeights(context.Background(), []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx1Included})
		if err == nil || !strings.Contains(err.Error(), "bad block") {
			t.Fatalf("expected index error, got %v", err)
		}
		deleted := indexer.deleted
		if len(deleted) != 1 || deleted[0] != mainnetfx.HeightEx1Included {
			t.Fatalf("failed block must be cleaned up, deleted %v", deleted)
		}
	})
}
