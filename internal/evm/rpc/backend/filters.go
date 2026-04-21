package backend

import (
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

// GetLogs returns all the logs from all the ethereum transactions in a block.
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

// GetLogsByHeight returns all the logs from all the ethereum transactions in a block.
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
