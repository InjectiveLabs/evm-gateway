package trace

import (
	"context"
	"fmt"
	"log/slog"

	"upd.dev/xlab/gotracer"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/debank"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

// API exposes Pipeline-compatible tracing methods under the trace namespace.
type API struct {
	logger  *slog.Logger
	backend backend.EVMBackend
}

// NewAPI creates the trace namespace service.
func NewAPI(logger *slog.Logger, evmBackend backend.EVMBackend) *API {
	return &API{
		logger:  logger.With("module", "trace"),
		backend: evmBackend,
	}
}

// DebankBlock replays a concrete block and returns Pipeline Mode 2's wire
// contract. The RPC server exposes this method as trace_debankBlock.
func (a *API) DebankBlock(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (*debank.Output, error) {
	defer gotracer.Trace(&ctx)()
	a.logger.Debug("trace_debankBlock", "block number or hash", blockNrOrHash)

	b := a.backend.WithContext(ctx)
	requestedHeight, err := b.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, fmt.Errorf("resolve block: %w", err)
	}
	if requestedHeight == rpctypes.EthPendingBlockNumber {
		return nil, fmt.Errorf("pending block is not traceable")
	}

	block, err := b.GetBlockByNumber(requestedHeight, true)
	if err != nil {
		return nil, fmt.Errorf("get block: %w", err)
	}
	if block == nil {
		return nil, fmt.Errorf("block not found")
	}
	height, err := debank.BlockHeight(block)
	if err != nil {
		return nil, err
	}
	blockRef := rpctypes.BlockNumberOrHash{BlockNumber: &height}

	receipts, err := b.GetBlockReceipts(blockRef)
	if err != nil {
		return nil, fmt.Errorf("get block receipts: %w", err)
	}
	traceResults, err := b.TraceBlock(height, debank.TraceConfig(), nil)
	if err != nil {
		return nil, fmt.Errorf("trace block: %w", err)
	}

	var parentBlock map[string]interface{}
	if height.Int64() > 1 {
		parentBlock, err = b.GetBlockByNumber(rpctypes.BlockNumber(height.Int64()-1), false)
		if err != nil {
			return nil, fmt.Errorf("get parent block: %w", err)
		}
		if parentBlock == nil {
			return nil, fmt.Errorf("parent block not found")
		}
	}

	return debank.Build(debank.BuildInput{
		Block:        block,
		ParentBlock:  parentBlock,
		Receipts:     receipts,
		TraceResults: traceResults,
		StateReader:  b,
	})
}
