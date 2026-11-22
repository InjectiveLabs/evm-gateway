package backend

import (
	"fmt"

	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

// traceTxView is the visible transaction list of a block, as
// eth_getBlockByNumber returns it, with the tx and receipt behind each hash.
type traceTxView struct {
	hashes []common.Hash
	lookup func(common.Hash) (*rpctypes.RPCTransaction, map[string]interface{}, error)
}

// traceBlockView returns the visible transactions of the block at height from
// the same source eth_getBlockByNumber uses: the indexer when the block is
// cached in the current virtualization mode and its cached tx list is
// consistent, otherwise (online) the live block data. resBlock and blockRes
// are the live data the caller already fetched, if any; missing ones are
// fetched only when the indexer cannot serve the view. It returns nil when no
// view is available.
func (b *Backend) traceBlockView(
	height int64,
	resBlock *cmrpctypes.ResultBlock,
	blockRes *cmrpctypes.ResultBlockResults,
) *traceTxView {
	if view := b.cachedTraceBlockView(height); view != nil {
		return view
	}
	if b.cfg.OfflineRPCOnly {
		return nil
	}
	if resBlock == nil || resBlock.Block == nil {
		var err error
		resBlock, err = b.TendermintBlockByNumber(rpctypes.BlockNumber(height))
		if err != nil || resBlock == nil || resBlock.Block == nil {
			return nil
		}
	}
	if blockRes == nil {
		if blockRes = b.blockResultsForTrace(resBlock.Block.Height); blockRes == nil {
			return nil
		}
	}

	if !b.virtualizationEnabled() {
		msgs := b.ethMsgsFromBlockResults(resBlock, blockRes)
		hashes := make([]common.Hash, 0, len(msgs))
		for _, msg := range msgs {
			hashes = append(hashes, msg.Hash())
		}
		return &traceTxView{hashes: hashes, lookup: b.liveTraceTx}
	}

	view, err := b.liveVirtualBlockView(resBlock, blockRes)
	if err != nil {
		b.logger.Debug("failed to build virtualized block view for block trace", "height", height, "error", err.Error())
		return nil
	}
	hashes := make([]common.Hash, 0, len(view.Transactions))
	txs := make(map[common.Hash]*rpctypes.RPCTransaction, len(view.Transactions))
	receipts := make(map[common.Hash]map[string]interface{}, len(view.Transactions))
	for i, tx := range view.Transactions {
		hashes = append(hashes, tx.Hash)
		txs[tx.Hash] = tx
		if i < len(view.Receipts) {
			receipts[tx.Hash] = view.Receipts[i]
		}
	}
	return &traceTxView{
		hashes: hashes,
		lookup: func(hash common.Hash) (*rpctypes.RPCTransaction, map[string]interface{}, error) {
			tx, receipt := txs[hash], receipts[hash]
			if tx == nil || receipt == nil {
				return nil, nil, fmt.Errorf("transaction %s not found in block view", hash.Hex())
			}
			return tx, receipt, nil
		},
	}
}

// cachedTraceBlockView returns the view of an indexed block, or nil when
// eth_getBlockByNumber would not serve the block from the indexer: not
// indexed, indexed in the other virtualization mode, or with an inconsistent
// tx list.
func (b *Backend) cachedTraceBlockView(height int64) *traceTxView {
	if b.indexer == nil {
		return nil
	}
	meta, err := b.indexer.GetBlockMetaByHeight(height)
	if err != nil || meta == nil || !b.cachedMetaMatchesVirtualization(meta) {
		return nil
	}
	txs, err := b.cachedBlockTransactions(meta, false)
	if err != nil {
		b.logger.Debug("cached block transactions unavailable for block trace", "height", height, "error", err.Error())
		return nil
	}
	hashes := make([]common.Hash, 0, len(txs))
	for _, tx := range txs {
		hash, ok := tx.(common.Hash)
		if !ok {
			return nil
		}
		hashes = append(hashes, hash)
	}
	return &traceTxView{hashes: hashes, lookup: b.cachedTraceTx}
}

// cachedTraceTx returns an indexed tx and its receipt.
func (b *Backend) cachedTraceTx(hash common.Hash) (*rpctypes.RPCTransaction, map[string]interface{}, error) {
	tx, err := b.indexer.GetRPCTransactionByHash(hash)
	if err != nil {
		return nil, nil, err
	}
	receipt, err := b.indexer.GetReceiptByTxHash(hash)
	if err != nil {
		return nil, nil, err
	}
	return tx, receipt, nil
}

// liveTraceTx returns a tx and its receipt as eth_getTransactionByHash and
// eth_getTransactionReceipt serve them.
func (b *Backend) liveTraceTx(hash common.Hash) (*rpctypes.RPCTransaction, map[string]interface{}, error) {
	tx, err := b.GetTransactionByHash(hash)
	if err != nil {
		return nil, nil, err
	}
	receipt, err := b.GetTransactionReceipt(hash)
	if err != nil {
		return nil, nil, err
	}
	if tx == nil || receipt == nil {
		return nil, nil, fmt.Errorf("transaction %s not found", hash.Hex())
	}
	return tx, receipt, nil
}

// syntheticTraceEntry returns the block trace entry of a visible tx built by
// syntheticTraceResult, or an entry-level error when its tx or receipt cannot
// be loaded. A visible Ethereum tx without errMsg is one whose trace is
// missing.
func (b *Backend) syntheticTraceEntry(view *traceTxView, hash common.Hash, config *rpctypes.TraceConfig, errMsg string) *rpctypes.TxTraceResult {
	entry := &rpctypes.TxTraceResult{TxHash: hash}
	tx, receipt, err := view.lookup(hash)
	if err != nil {
		b.logger.Debug("failed to load transaction for synthetic trace", "hash", hash.Hex(), "error", err.Error())
		entry.Error = traceUnavailableError
		return entry
	}
	if errMsg == "" && !tx.Virtual {
		errMsg = traceUnavailableError
	}
	result, err := syntheticTraceResult(tx, receipt, config, errMsg)
	if err != nil {
		b.logger.Debug("failed to build synthetic trace", "hash", hash.Hex(), "error", err.Error())
		entry.Error = traceUnavailableError
		return entry
	}
	entry.Result = result
	return entry
}

// alignTraceResults returns exactly one trace entry per visible tx of view,
// in visible order. Visible txs without a trace entry (virtual txs, and
// entries cached with the legacy placeholder) get a synthetic trace, and
// entries of txs that are not visible (e.g. failed in the ante handler) are
// dropped. Without a view the results are returned as they are.
func (b *Backend) alignTraceResults(
	results []*rpctypes.TxTraceResult,
	view *traceTxView,
	config *rpctypes.TraceConfig,
) []*rpctypes.TxTraceResult {
	if view == nil {
		return results
	}

	resultsByHash := make(map[common.Hash]*rpctypes.TxTraceResult, len(results))
	for _, result := range results {
		if result == nil || result.TxHash == (common.Hash{}) {
			return results
		}
		if _, ok := resultsByHash[result.TxHash]; !ok && !isLegacyTracePlaceholder(result) {
			resultsByHash[result.TxHash] = result
		}
	}

	aligned := make([]*rpctypes.TxTraceResult, 0, len(view.hashes))
	for _, hash := range view.hashes {
		if result, ok := resultsByHash[hash]; ok {
			aligned = append(aligned, result)
			continue
		}
		aligned = append(aligned, b.syntheticTraceEntry(view, hash, config, ""))
	}
	return aligned
}
