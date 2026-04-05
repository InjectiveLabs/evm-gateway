package backend

import (
	"context"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"upd.dev/xlab/gotracer"
)

func newBackendTraceTags() gotracer.Tags {
	return gotracer.NewTag("component", "evm_rpc_backend")
}

func (b *Backend) operationContext() context.Context {
	if b.ctx != nil {
		return b.ctx
	}
	return context.Background()
}

func (b *Backend) contextWithHeight(height int64) context.Context {
	return rpctypes.ContextWithHeightFrom(b.operationContext(), height)
}

func (b *Backend) WithContext(ctx context.Context) EVMBackend {
	clone := *b
	clone.ctx = ctx
	if clone.indexer != nil {
		clone.indexer = clone.indexer.WithContext(ctx)
	}
	return &clone
}
