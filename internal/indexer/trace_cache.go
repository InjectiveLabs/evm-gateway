package indexer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

const (
	KeyPrefixTraceTx    = 9
	KeyPrefixTraceBlock = 10
	traceConfigKeySize  = 32
)

func TraceTxKey(hash common.Hash, config *rpctypes.TraceConfig) []byte {
	key := make([]byte, 0, 1+common.HashLength+traceConfigKeySize)
	key = append(key, KeyPrefixTraceTx)
	key = append(key, hash.Bytes()...)
	key = append(key, traceConfigKey(config)...)
	return key
}

func TraceBlockKey(height int64, config *rpctypes.TraceConfig) []byte {
	key := make([]byte, 0, 1+8+traceConfigKeySize)
	key = append(key, KeyPrefixTraceBlock)
	key = append(key, sdk.Uint64ToBigEndian(uint64(height))...)
	key = append(key, traceConfigKey(config)...)
	return key
}

func traceTxPrefixStart(hash common.Hash) []byte {
	return append([]byte{KeyPrefixTraceTx}, hash.Bytes()...)
}

func traceBlockPrefixStart(height int64) []byte {
	return append([]byte{KeyPrefixTraceBlock}, sdk.Uint64ToBigEndian(uint64(height))...)
}

func prefixRangeEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] == 0xff {
			continue
		}
		end[i]++
		return end[:i+1]
	}
	return nil
}

func traceConfigKey(config *rpctypes.TraceConfig) []byte {
	bz, err := json.Marshal(config)
	if err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("marshal-error:%T", config)))
		return sum[:]
	}
	sum := sha256.Sum256(bz)
	return sum[:]
}

func (kv *KVIndexer) SetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig, raw json.RawMessage) error {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := kv.db.Set(TraceTxKey(hash, config), raw); err != nil {
		return errorsmod.Wrapf(err, "SetTraceTransaction %s", hash.Hex())
	}
	return nil
}

func (kv *KVIndexer) GetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(TraceTxKey(hash, config))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetTraceTransaction %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("trace tx not found, hash: %s", hash.Hex())
	}
	return append(json.RawMessage(nil), bz...), nil
}

func (kv *KVIndexer) SetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig, raw json.RawMessage) error {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	if len(raw) == 0 {
		raw = json.RawMessage("[]")
	}
	if err := kv.db.Set(TraceBlockKey(height, config), raw); err != nil {
		return errorsmod.Wrapf(err, "SetTraceBlockByHeight %d", height)
	}
	return nil
}

func (kv *KVIndexer) GetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(TraceBlockKey(height, config))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetTraceBlockByHeight %d", height)
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("trace block not found, height: %d", height)
	}
	return append(json.RawMessage(nil), bz...), nil
}

func (kv *KVIndexer) deleteTraceKeysForBlock(batch interface {
	Delete(key []byte) error
}, height int64, txHashes []common.Hash) error {
	for _, txHash := range txHashes {
		start := traceTxPrefixStart(txHash)
		it, err := kv.db.Iterator(start, prefixRangeEnd(start))
		if err != nil {
			return errorsmod.Wrapf(err, "delete tx trace iterator %d", height)
		}
		for ; it.Valid(); it.Next() {
			if err := batch.Delete(it.Key()); err != nil {
				it.Close()
				return errorsmod.Wrapf(err, "delete tx trace %d", height)
			}
		}
		it.Close()
	}

	start := traceBlockPrefixStart(height)
	it, err := kv.db.Iterator(start, prefixRangeEnd(start))
	if err != nil {
		return errorsmod.Wrapf(err, "delete block trace iterator %d", height)
	}
	defer it.Close()
	for ; it.Valid(); it.Next() {
		if err := batch.Delete(it.Key()); err != nil {
			return errorsmod.Wrapf(err, "delete block trace %d", height)
		}
	}
	return nil
}
