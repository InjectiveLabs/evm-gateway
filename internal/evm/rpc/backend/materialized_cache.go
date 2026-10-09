package backend

import (
	"github.com/ethereum/go-ethereum/common"
	lru "github.com/hashicorp/golang-lru"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

const (
	materializedReceiptCacheSize   = 8192
	materializedBlockLogsCacheSize = 512
)

type materializedCache struct {
	receipts  *lru.Cache
	blockLogs *lru.Cache
}

// newMaterializedCache creates small in-memory caches for decoded KV values
// that are expensive to materialize repeatedly during RPC reads.
func newMaterializedCache() *materializedCache {
	receipts, err := lru.New(materializedReceiptCacheSize)
	if err != nil {
		panic(err)
	}
	blockLogs, err := lru.New(materializedBlockLogsCacheSize)
	if err != nil {
		panic(err)
	}
	return &materializedCache{
		receipts:  receipts,
		blockLogs: blockLogs,
	}
}

// materializedEntry tags a cached value with the indexer cache epoch observed
// before its KV payload was read.
type materializedEntry struct {
	epoch uint64
	value interface{}
}

// getReceipt returns a previously decoded indexed receipt by transaction hash
// if it was cached during the current indexer cache epoch.
func (c *materializedCache) getReceipt(hash common.Hash, epoch uint64) (map[string]interface{}, bool) {
	if c == nil || c.receipts == nil {
		return nil, false
	}
	value, ok := getMaterialized(c.receipts, hash, epoch)
	if !ok {
		return nil, false
	}
	receipt, ok := value.(map[string]interface{})
	return receipt, ok
}

// addReceipt stores a decoded indexed receipt for reuse by cache-first RPC
// paths. epoch must be observed before the receipt was read from the KV store.
func (c *materializedCache) addReceipt(hash common.Hash, receipt map[string]interface{}, epoch uint64) {
	if c == nil || c.receipts == nil || receipt == nil {
		return
	}
	c.receipts.Add(hash, materializedEntry{epoch: epoch, value: receipt})
}

// getBlockLogs returns fully materialized logs for broad indexed log queries
// if they were cached during the current indexer cache epoch.
func (c *materializedCache) getBlockLogs(height int64, epoch uint64) ([]*virtualbank.RPCLog, bool) {
	if c == nil || c.blockLogs == nil {
		return nil, false
	}
	value, ok := getMaterialized(c.blockLogs, height, epoch)
	if !ok {
		return nil, false
	}
	logs, ok := value.([]*virtualbank.RPCLog)
	return logs, ok
}

// addBlockLogs stores fully materialized indexed logs for a broad block query.
// epoch must be observed before the logs were read from the KV store.
func (c *materializedCache) addBlockLogs(height int64, logs []*virtualbank.RPCLog, epoch uint64) {
	if c == nil || c.blockLogs == nil || logs == nil {
		return
	}
	c.blockLogs.Add(height, materializedEntry{epoch: epoch, value: logs})
}

// getMaterialized returns a cached value only when it was stored in the given
// epoch; entries from an older epoch may describe rewritten KV data and are
// evicted.
func getMaterialized(cache *lru.Cache, key interface{}, epoch uint64) (interface{}, bool) {
	raw, ok := cache.Get(key)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(materializedEntry)
	if !ok || entry.epoch != epoch {
		cache.Remove(key)
		return nil, false
	}
	return entry.value, true
}

// indexerCacheEpoch returns the indexer rewrite epoch used to validate
// in-memory caches of decoded KV payloads.
func (b *Backend) indexerCacheEpoch() uint64 {
	if source, ok := b.indexer.(txindexer.CacheEpochSource); ok {
		return source.CacheEpoch()
	}
	return 0
}
