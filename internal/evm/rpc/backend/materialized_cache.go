package backend

import (
	"github.com/ethereum/go-ethereum/common"
	lru "github.com/hashicorp/golang-lru"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
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

// getReceipt returns a previously decoded indexed receipt by transaction hash.
func (c *materializedCache) getReceipt(hash common.Hash) (map[string]interface{}, bool) {
	if c == nil || c.receipts == nil {
		return nil, false
	}
	value, ok := c.receipts.Get(hash)
	if !ok {
		return nil, false
	}
	receipt, ok := value.(map[string]interface{})
	return receipt, ok
}

// addReceipt stores a decoded indexed receipt for reuse by cache-first RPC
// paths.
func (c *materializedCache) addReceipt(hash common.Hash, receipt map[string]interface{}) {
	if c == nil || c.receipts == nil || receipt == nil {
		return
	}
	c.receipts.Add(hash, receipt)
}

// getBlockLogs returns fully materialized logs for broad indexed log queries.
func (c *materializedCache) getBlockLogs(height int64) ([]*virtualbank.RPCLog, bool) {
	if c == nil || c.blockLogs == nil {
		return nil, false
	}
	value, ok := c.blockLogs.Get(height)
	if !ok {
		return nil, false
	}
	logs, ok := value.([]*virtualbank.RPCLog)
	return logs, ok
}

// addBlockLogs stores fully materialized indexed logs for a broad block query.
func (c *materializedCache) addBlockLogs(height int64, logs []*virtualbank.RPCLog) {
	if c == nil || c.blockLogs == nil || logs == nil {
		return
	}
	c.blockLogs.Add(height, logs)
}
