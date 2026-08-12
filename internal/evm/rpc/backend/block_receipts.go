package backend

import (
	"fmt"

	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

// GetBlockReceipts returns all RPC-visible receipts for the provided block.
// It reads indexed receipts first and falls back to live Comet/gRPC data when
// the cache is missing or was built with a different virtualization mode.
func (b *Backend) GetBlockReceipts(blockNrOrHash rpctypes.BlockNumberOrHash) ([]map[string]interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	switch {
	case blockNrOrHash.BlockHash != nil && blockNrOrHash.BlockNumber != nil:
		return nil, fmt.Errorf("types BlockHash and BlockNumber cannot be both set")
	case blockNrOrHash.BlockHash == nil && blockNrOrHash.BlockNumber == nil:
		return nil, fmt.Errorf("types BlockHash and BlockNumber cannot be both nil")
	}

	if b.indexer != nil {
		receipts, err := b.cachedBlockReceipts(blockNrOrHash)
		if err == nil && receipts != nil {
			return receipts, nil
		}
		if err != nil && !isIndexerCacheMiss(err) {
			if b.cfg.OfflineRPCOnly {
				return nil, err
			}
			b.logger.Debug("cached block receipts lookup failed; falling back to live rpc", "block", blockNrOrHash, "error", err.Error())
		} else if b.cfg.OfflineRPCOnly {
			return nil, nil
		}
	}

	if b.cfg.OfflineRPCOnly {
		return nil, nil
	}

	resBlock, err := b.liveReceiptsBlock(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	if resBlock == nil || resBlock.Block == nil {
		return nil, nil
	}

	return b.liveBlockReceipts(resBlock)
}

// cachedBlockReceipts assembles block receipts only from indexed KV data.
// It is used by offline RPC mode and by online mode before trying live data.
func (b *Backend) cachedBlockReceipts(blockNrOrHash rpctypes.BlockNumberOrHash) ([]map[string]interface{}, error) {
	var (
		meta *txindexer.CachedBlockMeta
		err  error
	)

	switch {
	case blockNrOrHash.BlockHash != nil:
		meta, err = b.cachedBlockMetaByHash(*blockNrOrHash.BlockHash)
	case blockNrOrHash.BlockNumber != nil:
		meta, err = b.cachedBlockMetaByNumber(*blockNrOrHash.BlockNumber)
	default:
		return nil, fmt.Errorf("types BlockHash and BlockNumber cannot be both nil")
	}
	if err != nil || meta == nil {
		return nil, err
	}
	if !b.cachedMetaMatchesVirtualization(meta) {
		return nil, txindexer.ErrCacheMiss
	}
	if err := txindexer.ValidateCachedBlockMeta(meta); err != nil {
		return nil, err
	}
	if meta.EthTxCount == 0 {
		return []map[string]interface{}{}, nil
	}
	if b.indexer == nil {
		return nil, fmt.Errorf("cached block receipts unavailable without indexer for height %d", meta.Height)
	}

	hashes, err := b.indexer.GetRPCTransactionHashesByBlockHeight(meta.Height)
	if err != nil {
		return nil, err
	}
	if len(hashes) != int(meta.EthTxCount) {
		return nil, fmt.Errorf(
			"cached block receipt count mismatch at height %d: expected %d got %d",
			meta.Height,
			meta.EthTxCount,
			len(hashes),
		)
	}

	receipts := make([]map[string]interface{}, 0, len(hashes))
	for _, hash := range hashes {
		if hash == (common.Hash{}) {
			return nil, fmt.Errorf("cached block tx hash missing for height %d", meta.Height)
		}
		receipt, err := b.materializedReceiptByHash(hash)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}

	return receipts, nil
}

// materializedReceiptByHash returns an indexed receipt, using the in-memory
// materialized cache to avoid decoding the same KV payload repeatedly.
func (b *Backend) materializedReceiptByHash(hash common.Hash) (map[string]interface{}, error) {
	if receipt, ok := b.materialized.getReceipt(hash); ok {
		return receipt, nil
	}
	receipt, err := b.indexer.GetReceiptByTxHash(hash)
	if err != nil {
		return nil, err
	}
	b.materialized.addReceipt(hash, receipt)
	return receipt, nil
}

// liveReceiptsBlock resolves the requested block through live Comet RPC.
func (b *Backend) liveReceiptsBlock(blockNrOrHash rpctypes.BlockNumberOrHash) (*cmrpctypes.ResultBlock, error) {
	switch {
	case blockNrOrHash.BlockHash != nil:
		return b.TendermintBlockByHash(*blockNrOrHash.BlockHash)
	case blockNrOrHash.BlockNumber != nil:
		return b.TendermintBlockByNumber(*blockNrOrHash.BlockNumber)
	default:
		return nil, fmt.Errorf("types BlockHash and BlockNumber cannot be both nil")
	}
}

// liveBlockReceipts builds every RPC-visible receipt from live Comet block
// results, including IBC hook executions and optional virtual bank transactions.
func (b *Backend) liveBlockReceipts(resBlock *cmrpctypes.ResultBlock) ([]map[string]interface{}, error) {
	if resBlock == nil || resBlock.Block == nil {
		return nil, nil
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		b.logger.Warn("failed to retrieve block results", "height", resBlock.Block.Height, "error", err.Error())
		return nil, nil
	}
	if blockRes == nil {
		return nil, nil
	}

	view, err := b.liveVirtualBankBlockView(resBlock, blockRes)
	if err != nil {
		return nil, err
	}
	return view.Receipts, nil
}
