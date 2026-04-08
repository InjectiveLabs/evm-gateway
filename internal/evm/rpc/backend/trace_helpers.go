package backend

import (
	"context"
	"math/big"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
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

func traceChainID(fallback *big.Int, msgs ...*evmtypes.MsgEthereumTx) int64 {
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		tx := msg.AsTransaction()
		if tx == nil || tx.ChainId() == nil || tx.ChainId().Sign() <= 0 {
			continue
		}
		return tx.ChainId().Int64()
	}
	if fallback == nil {
		return 0
	}
	return fallback.Int64()
}
