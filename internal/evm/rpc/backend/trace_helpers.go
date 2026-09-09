package backend

import (
	"context"
	"math/big"

	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"google.golang.org/grpc/metadata"

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
	ctx := b.operationContext()
	if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md.Get(grpctypes.GRPCBlockHeightHeader)) != 0 {
		md = md.Copy()
		md.Delete(grpctypes.GRPCBlockHeightHeader)
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	ctx = rpctypes.ContextWithHeightFrom(ctx, height)
	return withSelectedBackend(ctx, height)
}

func (b *Backend) contextForCometHeight(height int64) context.Context {
	return withSelectedBackend(b.operationContext(), height)
}

func withSelectedBackend(ctx context.Context, height int64) context.Context {
	backend := earliestBackendAt(ctx, height)
	md, _ := metadata.FromOutgoingContext(ctx)
	if backend == "" && len(md.Get("x-stitch-backend")) == 0 && len(md.Get("stitch")) == 0 &&
		len(md.Get("x-stitch-earliest-height")) == 0 && len(md.Get("x-stitch-earliest-capability")) == 0 {
		return ctx
	}
	md = md.Copy()
	md.Delete("stitch")
	md.Delete("x-stitch-backend")
	md.Delete("x-stitch-earliest-height")
	md.Delete("x-stitch-earliest-capability")
	if backend != "" {
		md.Set("x-stitch-backend", backend)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func (b *Backend) WithContext(ctx context.Context) EVMBackend {
	clone := *b
	if b.cfg.StitchBackend {
		ctx = withEarliestSelections(ctx)
	}
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
