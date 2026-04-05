package indexer

import (
	"context"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"upd.dev/xlab/gotracer"
)

func newIndexerTraceTags() gotracer.Tags {
	return gotracer.NewTag("component", "tx_indexer")
}

func (kv *KVIndexer) operationContext() context.Context {
	if kv.ctx != nil {
		return kv.ctx
	}
	return context.Background()
}

func (kv *KVIndexer) contextWithHeight(height int64) context.Context {
	return rpctypes.ContextWithHeightFrom(kv.operationContext(), height)
}

func (kv *KVIndexer) WithContext(ctx context.Context) TxIndexer {
	clone := *kv
	clone.ctx = ctx
	return &clone
}
