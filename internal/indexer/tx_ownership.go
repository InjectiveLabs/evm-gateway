package indexer

import (
	"sync/atomic"

	errorsmod "cosmossdk.io/errors"
	"github.com/ethereum/go-ethereum/common"

	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

// CacheEpochSource is implemented by indexers that can tell readers when
// previously indexed data was rewritten or removed. Any in-memory cache of
// decoded KV payloads must be discarded once the epoch changes.
type CacheEpochSource interface {
	CacheEpoch() uint64
}

var _ CacheEpochSource = &KVIndexer{}

// cacheEpoch is shared by all WithContext clones of a KVIndexer.
type cacheEpoch struct {
	value atomic.Uint64
}

// CacheEpoch returns the current rewrite epoch. It changes only when indexing
// replaces or removes data that was already stored, never for appends of new
// blocks.
func (kv *KVIndexer) CacheEpoch() uint64 {
	if kv.epoch == nil {
		return 0
	}
	return kv.epoch.value.Load()
}

func (kv *KVIndexer) bumpCacheEpoch() {
	if kv.epoch != nil {
		kv.epoch.value.Add(1)
	}
}

// txHashOwnerHeight returns the height of the block that currently owns the
// hash-keyed records (tx result, rpc tx, receipt) of the given tx hash.
// Undecodable payloads and payloads without a height are reported as missing
// so they can be overwritten.
func (kv *KVIndexer) txHashOwnerHeight(hash common.Hash) (int64, bool, error) {
	bz, err := kv.db.Get(TxHashKey(hash))
	if err != nil {
		return 0, false, errorsmod.Wrapf(err, "get tx result %s", hash.Hex())
	}
	if len(bz) > 0 {
		if res, ok := kv.decodeTxResultForOwnership(bz); ok && res.Height > 0 {
			return res.Height, true, nil
		}
	}

	bz, err = kv.db.Get(RPCtxHashKey(hash))
	if err != nil {
		return 0, false, errorsmod.Wrapf(err, "get rpc tx %s", hash.Hex())
	}
	if len(bz) > 0 {
		if tx, err := unmarshalRPCTransactionPayload(bz); err == nil && tx != nil && tx.BlockNumber != nil && tx.BlockNumber.ToInt().Sign() > 0 {
			return tx.BlockNumber.ToInt().Int64(), true, nil
		}
	}

	bz, err = kv.db.Get(ReceiptKey(hash))
	if err != nil {
		return 0, false, errorsmod.Wrapf(err, "get receipt %s", hash.Hex())
	}
	if len(bz) > 0 {
		if receipt, err := unmarshalReceiptPayload(bz); err == nil && receipt.BlockNumber > 0 {
			return int64(receipt.BlockNumber), true, nil
		}
	}

	return 0, false, nil
}

// txHashOwnedByHeight reports whether the hash-keyed records of a tx belong to
// the given height, or are absent (in which case deleting them is a no-op).
func (kv *KVIndexer) txHashOwnedByHeight(hash common.Hash, height int64) (bool, error) {
	owner, found, err := kv.txHashOwnerHeight(hash)
	if err != nil {
		return false, err
	}
	return !found || owner == height, nil
}

func (kv *KVIndexer) decodeTxResultForOwnership(bz []byte) (res *chaintypes.TxResult, ok bool) {
	defer func() {
		// the legacy protobuf format needs a codec; treat a missing codec as an
		// undecodable payload instead of crashing the indexer.
		if recover() != nil {
			res, ok = nil, false
		}
	}()
	res, err := unmarshalTxResultPayload(kv.clientCtx.Codec, bz)
	if err != nil || res == nil {
		return nil, false
	}
	return res, true
}
