package backend

import (
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

type filteredLogIndexer interface {
	GetFilteredLogsByBlockHeight(height int64, addresses []common.Address, topics [][]common.Hash) ([]*virtualbank.RPCLog, error)
	GetFilteredLogsByBlockHash(hash common.Hash, addresses []common.Address, topics [][]common.Hash) ([]*virtualbank.RPCLog, error)
}

// GetLogs returns all RPC-visible logs for a block hash. It prefers indexed KV
// logs, but only when the cached block was indexed with the current
// virtualization setting; otherwise online mode falls back to live block
// results.
func (b *Backend) GetLogs(hash common.Hash) ([][]*virtualbank.RPCLog, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		meta, metaErr := b.indexer.GetBlockMetaByHash(hash)
		if metaErr == nil && meta != nil && b.cachedMetaMatchesVirtualization(meta) {
			logs, err := b.indexer.GetLogsByBlockHash(hash)
			if err == nil {
				if b.syncStatus != nil {
					b.syncStatus.RecordBlockLogsCacheHit()
				}
				return logs, nil
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		} else if metaErr == nil && meta != nil && !b.cachedMetaMatchesVirtualization(meta) {
			if b.cfg.OfflineRPCOnly {
				return nil, errors.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		}
	}

	resBlock, err := b.TendermintBlockByHash(hash)
	if err != nil {
		return nil, err
	}
	if resBlock == nil {
		return nil, errors.Errorf("block not found for hash %s", hash)
	}
	return b.GetLogsByHeight(&resBlock.Block.Header.Height)
}

// GetFilteredLogs returns logs for a block hash after applying Ethereum address
// and topic filtering. Indexed Cap'n Proto payloads can be filtered before full
// materialization; live fallback is used when cached data is unavailable or was
// indexed with a different virtualization mode.
func (b *Backend) GetFilteredLogs(
	hash common.Hash,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]*virtualbank.RPCLog, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		meta, metaErr := b.indexer.GetBlockMetaByHash(hash)
		if metaErr == nil && meta != nil && b.cachedMetaMatchesVirtualization(meta) {
			if filtered, ok := b.indexer.(filteredLogIndexer); ok {
				if broadLogFilter(addresses, topics) {
					if logs, ok := b.materialized.getBlockLogs(meta.Height); ok {
						if b.syncStatus != nil {
							b.syncStatus.RecordBlockLogsCacheHit()
						}
						return logs, nil
					}
				}
				logs, err := filtered.GetFilteredLogsByBlockHash(hash, addresses, topics)
				if err == nil {
					if broadLogFilter(addresses, topics) {
						b.materialized.addBlockLogs(meta.Height, logs)
					}
					if b.syncStatus != nil {
						b.syncStatus.RecordBlockLogsCacheHit()
					}
					return logs, nil
				}
				if b.syncStatus != nil {
					b.syncStatus.RecordBlockLogsCacheMiss()
					b.syncStatus.RecordBlockLogsLiveFallback()
				}
			}
		} else if metaErr == nil && meta != nil && !b.cachedMetaMatchesVirtualization(meta) {
			if b.cfg.OfflineRPCOnly {
				return nil, errors.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		}
	}

	logsList, err := b.GetLogs(hash)
	if err != nil {
		return nil, err
	}
	return filterGroupedLogs(logsList, addresses, topics), nil
}

// GetLogsByHeight returns all RPC-visible logs for a block height. It reads the
// indexed cache first and builds a live view from Comet block results when the
// cache is missing, stale for the current virtualization mode, or unavailable.
func (b *Backend) GetLogsByHeight(height *int64) ([][]*virtualbank.RPCLog, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		meta, metaErr := b.indexer.GetBlockMetaByHeight(*height)
		if metaErr == nil && meta != nil && b.cachedMetaMatchesVirtualization(meta) {
			logs, err := b.indexer.GetLogsByBlockHeight(*height)
			if err == nil {
				if b.syncStatus != nil {
					b.syncStatus.RecordBlockLogsCacheHit()
				}
				return logs, nil
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		} else if metaErr == nil && meta != nil && !b.cachedMetaMatchesVirtualization(meta) {
			if b.cfg.OfflineRPCOnly {
				return nil, errors.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		}
	}

	// NOTE: we query the state in case the tx result logs are not persisted after an upgrade.
	blockRes, err := b.TendermintBlockResultByNumber(height)
	if err != nil {
		return nil, err
	}

	if b.virtualBankEnabled() {
		resBlock, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(*height))
		if err != nil {
			return nil, err
		}
		if resBlock == nil || resBlock.Block == nil {
			return nil, errors.Errorf("block not found for height %d", *height)
		}
		view, err := b.liveVirtualBankBlockView(resBlock, blockRes)
		if err != nil {
			return nil, err
		}
		return view.Logs, nil
	}

	return GetLogsFromBlockResults(blockRes)
}

// GetFilteredLogsByHeight returns filtered logs for a block height using
// indexed KV filtering when possible and live block-result filtering otherwise.
func (b *Backend) GetFilteredLogsByHeight(
	height int64,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]*virtualbank.RPCLog, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		meta, metaErr := b.indexer.GetBlockMetaByHeight(height)
		if metaErr == nil && meta != nil && b.cachedMetaMatchesVirtualization(meta) {
			if filtered, ok := b.indexer.(filteredLogIndexer); ok {
				if broadLogFilter(addresses, topics) {
					if logs, ok := b.materialized.getBlockLogs(height); ok {
						if b.syncStatus != nil {
							b.syncStatus.RecordBlockLogsCacheHit()
						}
						return logs, nil
					}
				}
				logs, err := filtered.GetFilteredLogsByBlockHeight(height, addresses, topics)
				if err == nil {
					if broadLogFilter(addresses, topics) {
						b.materialized.addBlockLogs(height, logs)
					}
					if b.syncStatus != nil {
						b.syncStatus.RecordBlockLogsCacheHit()
					}
					return logs, nil
				}
				if b.syncStatus != nil {
					b.syncStatus.RecordBlockLogsCacheMiss()
					b.syncStatus.RecordBlockLogsLiveFallback()
				}
			}
		} else if metaErr == nil && meta != nil && !b.cachedMetaMatchesVirtualization(meta) {
			if b.cfg.OfflineRPCOnly {
				return nil, errors.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
			}
			if b.syncStatus != nil {
				b.syncStatus.RecordBlockLogsCacheMiss()
				b.syncStatus.RecordBlockLogsLiveFallback()
			}
		}
	}

	logsList, err := b.GetLogsByHeight(&height)
	if err != nil {
		return nil, err
	}
	return filterGroupedLogs(logsList, addresses, topics), nil
}

// broadLogFilter reports whether a log query has no address or topic clauses
// and can safely reuse a whole-block materialized log cache entry.
func broadLogFilter(addresses []common.Address, topics [][]common.Hash) bool {
	if len(addresses) > 0 {
		return false
	}
	for _, group := range topics {
		if len(group) > 0 {
			return false
		}
	}
	return true
}

// filterGroupedLogs flattens grouped per-transaction logs and applies Ethereum
// address/topic matching.
func filterGroupedLogs(
	logsList [][]*virtualbank.RPCLog,
	addresses []common.Address,
	topics [][]common.Hash,
) []*virtualbank.RPCLog {
	out := make([]*virtualbank.RPCLog, 0)
	for _, txLogs := range logsList {
		for _, log := range txLogs {
			if virtualbank.LogMatches(log, addresses, topics) {
				out = append(out, log)
			}
		}
	}
	if out == nil {
		return []*virtualbank.RPCLog{}
	}
	return out
}

func (b *Backend) GetBlockBloomByHeight(height int64) (ethtypes.Bloom, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		meta, err := b.indexer.GetBlockMetaByHeight(height)
		if err == nil && b.cachedMetaMatchesVirtualization(meta) {
			return ethtypes.BytesToBloom(common.FromHex(meta.Bloom)), nil
		}
	}

	blockRes, err := b.TendermintBlockResultByNumber(&height)
	if err != nil {
		return ethtypes.Bloom{}, err
	}
	if blockRes == nil {
		return ethtypes.Bloom{}, errors.Errorf("block result not found for height %d", height)
	}

	return b.BlockBloom(blockRes)
}

// BloomStatus returns the BloomBitsBlocks and the number of processed sections maintained
// by the chain indexer.
func (b *Backend) BloomStatus() (uint64, uint64) {
	return 4096, 0
}
