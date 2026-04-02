package indexer

import (
	"encoding/json"
	"fmt"
	"log/slog"

	dbm "github.com/cosmos/cosmos-db"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
)

const clearBatchSize = 500

// ClearBlockRange deletes all indexed data for blocks in the inclusive range
// [from, to] from the given DB. After this call the blocks appear as gaps to
// the syncer, which will re-index them on the next startup.
func ClearBlockRange(db dbm.DB, logger *slog.Logger, from, to int64) error {
	if from > to {
		return fmt.Errorf("from (%d) must be <= to (%d)", from, to)
	}

	logger.Info("clearing indexed block range", "from", from, "to", to)

	// Step 1: delete height-keyed entries and collect block hashes for cleanup.
	// We iterate in chunks so the batch stays manageable.
	blockHashes := make([]common.Hash, 0)
	for start := from; start <= to; start += clearBatchSize {
		end := start + clearBatchSize - 1
		if end > to {
			end = to
		}
		hashes, err := clearHeightKeys(db, start, end)
		if err != nil {
			return fmt.Errorf("clearing height keys [%d, %d]: %w", start, end, err)
		}
		blockHashes = append(blockHashes, hashes...)
		logger.Info("cleared height-keyed entries", "start", start, "end", end)
	}

	// Step 2: delete BlockHash entries (hash → height mapping).
	if err := clearBlockHashKeys(db, blockHashes); err != nil {
		return fmt.Errorf("clearing block hash keys: %w", err)
	}

	// Step 3: scan TxIndex (block+ethTxIndex → txHash) to find and delete all
	// tx-keyed entries: TxHash, TxIndex, RPCtxHash, RPCtxIndex, Receipt.
	for start := from; start <= to; start += clearBatchSize {
		end := start + clearBatchSize - 1
		if end > to {
			end = to
		}
		if err := clearTxKeys(db, start, end); err != nil {
			return fmt.Errorf("clearing tx keys [%d, %d]: %w", start, end, err)
		}
		logger.Info("cleared tx-keyed entries", "start", start, "end", end)
	}

	logger.Info("block range cleared; restart the gateway to re-index", "from", from, "to", to)
	return nil
}

// clearHeightKeys deletes BlockMeta and BlockLogs for [from, to] in a single
// batch. It also returns the block hashes found in the BlockMeta entries so
// BlockHash keys can be cleaned up in a second pass.
func clearHeightKeys(db dbm.DB, from, to int64) ([]common.Hash, error) {
	batch := db.NewBatch()
	defer batch.Close()

	var blockHashes []common.Hash

	for height := from; height <= to; height++ {
		metaKey := BlockMetaKey(height)
		bz, err := db.Get(metaKey)
		if err == nil && len(bz) > 0 {
			var meta CachedBlockMeta
			if jsonErr := json.Unmarshal(bz, &meta); jsonErr == nil && meta.Hash != "" {
				blockHashes = append(blockHashes, common.HexToHash(meta.Hash))
			}
			if err := batch.Delete(metaKey); err != nil {
				return nil, err
			}
		}

		if err := batch.Delete(BlockLogsKey(height)); err != nil {
			return nil, err
		}
	}

	return blockHashes, batch.WriteSync()
}

// clearBlockHashKeys deletes BlockHash (hash → height) entries.
func clearBlockHashKeys(db dbm.DB, hashes []common.Hash) error {
	if len(hashes) == 0 {
		return nil
	}
	batch := db.NewBatch()
	defer batch.Close()
	for _, h := range hashes {
		if err := batch.Delete(BlockHashKey(h)); err != nil {
			return err
		}
	}
	return batch.WriteSync()
}

// clearTxKeys scans TxIndex entries for [from, to] and deletes the
// corresponding TxHash, TxIndex, RPCtxHash, RPCtxIndex, and Receipt keys.
func clearTxKeys(db dbm.DB, from, to int64) error {
	startKey := txIndexRangeStart(from)
	endKey := txIndexRangeStart(to + 1)

	it, err := db.Iterator(startKey, endKey)
	if err != nil {
		return err
	}
	defer it.Close()

	batch := db.NewBatch()
	defer batch.Close()

	for ; it.Valid(); it.Next() {
		txHash := common.BytesToHash(it.Value())

		// Delete TxHash (proto tx result) and the TxIndex entry itself.
		if err := batch.Delete(TxHashKey(txHash)); err != nil {
			return err
		}
		if err := batch.Delete(it.Key()); err != nil {
			return err
		}
	}
	it.Close()

	// Also scan RPCtxIndex for the same range to clean up cached RPC tx /
	// receipt entries.
	rpctxStart := rpctxIndexRangeStart(from)
	rpctxEnd := rpctxIndexRangeStart(to + 1)

	rpcIt, err := db.Iterator(rpctxStart, rpctxEnd)
	if err != nil {
		return err
	}
	defer rpcIt.Close()

	for ; rpcIt.Valid(); rpcIt.Next() {
		txHash := common.BytesToHash(rpcIt.Value())

		if err := batch.Delete(RPCtxHashKey(txHash)); err != nil {
			return err
		}
		if err := batch.Delete(ReceiptKey(txHash)); err != nil {
			return err
		}
		if err := batch.Delete(rpcIt.Key()); err != nil {
			return err
		}
	}

	return batch.WriteSync()
}

// txIndexRangeStart builds the inclusive start key for TxIndex range scans.
func txIndexRangeStart(height int64) []byte {
	return append([]byte{KeyPrefixTxIndex}, sdk.Uint64ToBigEndian(uint64(height))...)
}

// rpctxIndexRangeStart builds the inclusive start key for RPCtxIndex range scans.
func rpctxIndexRangeStart(height int64) []byte {
	return append([]byte{KeyPrefixRPCtxIndex}, sdk.Uint64ToBigEndian(uint64(height))...)
}
