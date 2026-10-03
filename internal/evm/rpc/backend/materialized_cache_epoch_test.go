package backend

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func receiptBlockAndStatus(t *testing.T, receipt map[string]interface{}) (uint64, uint) {
	t.Helper()
	if receipt == nil {
		t.Fatalf("nil receipt")
	}
	return uint64(receipt["blockNumber"].(hexutil.Uint64)), uint(receipt["status"].(hexutil.Uint))
}

// TestReceiptCacheDropsStaleReceiptAfterReindex reproduces the incident: the
// receipt of the ante-failed inclusion was cached in memory and kept being
// served after the later inclusion rewrote the indexed record.
func TestReceiptCacheDropsStaleReceiptAfterReindex(t *testing.T) {
	kv, _ := newLegacyFixtureKV(t)
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: kv})
	included := mainnetfx.HeightEx1Included
	blockNr := rpctypes.BlockNumber(included)

	receipt, err := b.GetTransactionReceipt(mainnetfx.TxEx1)
	if err != nil {
		t.Fatalf("GetTransactionReceipt: %v", err)
	}
	if height, status := receiptBlockAndStatus(t, receipt); height != uint64(mainnetfx.HeightEx1Failed) || status != 0 {
		t.Fatalf("legacy state must serve the stale receipt: block %d status %d", height, status)
	}
	receipts, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNr})
	if err != nil || len(receipts) != 1 {
		t.Fatalf("GetBlockReceipts: %v %v", receipts, err)
	}
	if height, _ := receiptBlockAndStatus(t, receipts[0]); height != uint64(mainnetfx.HeightEx1Failed) {
		t.Fatalf("legacy state must label the block receipt with the stale block: %d", height)
	}

	epoch := kv.CacheEpoch()
	indexFixtureBlocks(t, kv, included)
	if kv.CacheEpoch() == epoch {
		t.Fatalf("reindexing a block must bump the cache epoch")
	}

	receipt, err = b.GetTransactionReceipt(mainnetfx.TxEx1)
	if err != nil {
		t.Fatalf("GetTransactionReceipt: %v", err)
	}
	if height, status := receiptBlockAndStatus(t, receipt); height != uint64(included) || status != 1 {
		t.Fatalf("stale receipt served after reindex: block %d status %d", height, status)
	}
	receipts, err = b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNr})
	if err != nil || len(receipts) != 1 {
		t.Fatalf("GetBlockReceipts: %v %v", receipts, err)
	}
	if height, status := receiptBlockAndStatus(t, receipts[0]); height != uint64(included) || status != 1 {
		t.Fatalf("stale block receipt after reindex: block %d status %d", height, status)
	}
}

func TestMaterializedCacheEpochs(t *testing.T) {
	hash := common.HexToHash("0x01")
	receipt := map[string]interface{}{"status": hexutil.Uint(1)}
	logs := []*virtualbank.RPCLog{{}}

	c := newMaterializedCache()
	c.addReceipt(hash, receipt, 3)
	if got, ok := c.getReceipt(hash, 3); !ok || got["status"] != receipt["status"] {
		t.Fatalf("receipt must be served within its epoch")
	}
	if _, ok := c.getReceipt(hash, 4); ok {
		t.Fatalf("receipt must not be served in another epoch")
	}
	if c.receipts.Contains(hash) {
		t.Fatalf("stale receipt entry must be evicted")
	}
	c.addReceipt(hash, nil, 4)
	if c.receipts.Contains(hash) {
		t.Fatalf("nil receipt must not be cached")
	}
	c.receipts.Add(hash, receipt)
	if _, ok := c.getReceipt(hash, 0); ok {
		t.Fatalf("raw values without an epoch must be ignored")
	}
	c.receipts.Add(hash, materializedEntry{epoch: 5, value: "not a receipt"})
	if _, ok := c.getReceipt(hash, 5); ok {
		t.Fatalf("wrongly typed receipt must be ignored")
	}

	c.addBlockLogs(7, logs, 1)
	if got, ok := c.getBlockLogs(7, 1); !ok || len(got) != 1 {
		t.Fatalf("logs must be served within their epoch")
	}
	if _, ok := c.getBlockLogs(7, 2); ok {
		t.Fatalf("logs must not be served in another epoch")
	}
	if _, ok := c.getBlockLogs(8, 2); ok {
		t.Fatalf("unexpected logs for unknown height")
	}
	c.addBlockLogs(7, nil, 2)
	if c.blockLogs.Contains(int64(7)) {
		t.Fatalf("nil logs must not be cached")
	}
	c.blockLogs.Add(int64(7), materializedEntry{epoch: 2, value: "not logs"})
	if _, ok := c.getBlockLogs(7, 2); ok {
		t.Fatalf("wrongly typed logs must be ignored")
	}

	var nilCache *materializedCache
	nilCache.addReceipt(hash, receipt, 0)
	nilCache.addBlockLogs(7, logs, 0)
	if _, ok := nilCache.getReceipt(hash, 0); ok {
		t.Fatalf("nil cache must miss")
	}
	if _, ok := nilCache.getBlockLogs(7, 0); ok {
		t.Fatalf("nil cache must miss")
	}
}

func TestIndexerCacheEpochSources(t *testing.T) {
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true})
	if got := b.indexerCacheEpoch(); got != 0 {
		t.Fatalf("nil indexer epoch: %d", got)
	}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: &overrideIndexer{}})
	if got := b.indexerCacheEpoch(); got != 0 {
		t.Fatalf("indexer without epochs: %d", got)
	}
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, mainnetfx.HeightEx1Failed, mainnetfx.HeightEx1Failed)
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: kv})
	if got := b.indexerCacheEpoch(); got == 0 || got != kv.CacheEpoch() {
		t.Fatalf("unexpected epoch %d (indexer %d)", got, kv.CacheEpoch())
	}
}

// TestBlockLogsCacheEpoch covers the epoch threading of both indexed log
// paths: a cached entry is served within the epoch and ignored after a
// rewrite of the block.
func TestBlockLogsCacheEpoch(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: kv})
	blockHash := common.BytesToHash(mustFixtureResultBlock(t, height).BlockID.Hash)

	real, err := b.GetFilteredLogsByHeight(height, nil, nil)
	if err != nil {
		t.Fatalf("GetFilteredLogsByHeight: %v", err)
	}
	if _, ok := b.materialized.getBlockLogs(height, kv.CacheEpoch()); !ok {
		t.Fatalf("broad log query must populate the cache")
	}

	poisoned := []*virtualbank.RPCLog{{}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}}
	if len(poisoned) == len(real) {
		t.Fatalf("poisoned logs must differ from real logs")
	}
	b.materialized.addBlockLogs(height, poisoned, kv.CacheEpoch())

	byHeight, err := b.GetFilteredLogsByHeight(height, nil, nil)
	if err != nil || len(byHeight) != len(poisoned) {
		t.Fatalf("by height: cached logs must be served within the epoch: %d %v", len(byHeight), err)
	}
	byHash, err := b.GetFilteredLogs(blockHash, nil, nil)
	if err != nil || len(byHash) != len(poisoned) {
		t.Fatalf("by hash: cached logs must be served within the epoch: %d %v", len(byHash), err)
	}

	indexFixtureBlocks(t, kv, height) // rewrite bumps the epoch
	byHeight, err = b.GetFilteredLogsByHeight(height, nil, nil)
	if err != nil || len(byHeight) != len(real) {
		t.Fatalf("by height: stale cached logs served after rewrite: %d %v", len(byHeight), err)
	}
	b.materialized.addBlockLogs(height, poisoned, kv.CacheEpoch()-1)
	byHash, err = b.GetFilteredLogs(blockHash, nil, nil)
	if err != nil || len(byHash) != len(real) {
		t.Fatalf("by hash: stale cached logs served after rewrite: %d %v", len(byHash), err)
	}
}
