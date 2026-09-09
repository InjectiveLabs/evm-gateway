package backend

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type earliestSelectionKey struct{}

type earliestSelections struct {
	mu       sync.RWMutex
	byHeight map[int64]string
}

func withEarliestSelections(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(earliestSelectionKey{}).(*earliestSelections); ok {
		return ctx
	}
	return context.WithValue(ctx, earliestSelectionKey{}, &earliestSelections{byHeight: make(map[int64]string)})
}

func earliestBackendAt(ctx context.Context, height int64) string {
	selections, ok := ctx.Value(earliestSelectionKey{}).(*earliestSelections)
	if !ok {
		return ""
	}
	selections.mu.RLock()
	defer selections.mu.RUnlock()
	return selections.byHeight[height]
}

func (b *Backend) hasEarliestSelection(height int64) bool {
	return earliestBackendAt(b.operationContext(), height) != ""
}

// Optional EVM fields can legitimately be absent at activation. Only transport
// failures override their existing domain-specific defaulting behavior.
func isGRPCAvailabilityFailure(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return err == context.Canceled || err == context.DeadlineExceeded
	}
}

const (
	earliestState     = "state"
	earliestBlock     = "block"
	earliestExecution = "execution"
	earliestProof     = "proof"
	earliestRange     = "range"
	earliestTrace     = "trace"
	earliestStorage   = "storage"
)

// ResolveEarliestBlockNumber resolves the symbolic earliest selector before any
// cache or dependent upstream reads. Numeric zero remains its existing alias;
// positive Cosmos heights, including height one, are never rewritten.
//
// The routing marker belongs only to this discovery call. The returned concrete
// height is passed through normal Comet/gRPC paths, including derived heights
// such as the parent state needed for tracing a block.
func (b *Backend) ResolveEarliestBlockNumber(blockNum rpctypes.BlockNumber, capability string) (rpctypes.BlockNumber, error) {
	if !b.cfg.StitchBackend || blockNum != rpctypes.EthEarliestBlockNumber {
		return blockNum, nil
	}
	if b.queryClient == nil || b.queryClient.QueryClient == nil {
		return 0, fmt.Errorf("Stitch earliest discovery requires a gRPC client")
	}
	ctx := b.operationContext()
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Delete("x-stitch-backend")
	md.Delete("x-stitch-earliest-height")
	md.Set("stitch", "earliest")
	md.Set("x-stitch-earliest-capability", capability)
	// Retain the old placeholder so an older Stitch cannot quietly serve latest.
	md.Set(grpctypes.GRPCBlockHeightHeader, "1")
	ctx = metadata.NewOutgoingContext(ctx, md)
	var headers metadata.MD
	_, err := b.queryClient.Params(ctx, &evmtypes.QueryParamsRequest{}, grpc.Header(&headers))
	if err != nil {
		return 0, fmt.Errorf("resolve earliest EVM history: %w", err)
	}
	resolved := headers.Get("x-stitch-earliest-height")
	actual := headers.Get(grpctypes.GRPCBlockHeightHeader)
	if len(resolved) != 1 || len(actual) != 1 || resolved[0] != actual[0] {
		return 0, fmt.Errorf("Stitch earliest discovery returned missing or inconsistent height metadata")
	}
	height, err := strconv.ParseInt(resolved[0], 10, 64)
	if err != nil || height <= 0 {
		return 0, fmt.Errorf("Stitch earliest discovery returned invalid height %q", resolved[0])
	}
	selected := headers.Get("x-stitch-backend")
	if len(selected) != 1 || selected[0] == "" {
		return 0, fmt.Errorf("Stitch earliest discovery returned missing or ambiguous backend metadata")
	}
	if selections, ok := b.operationContext().Value(earliestSelectionKey{}).(*earliestSelections); ok {
		selections.mu.Lock()
		selections.byHeight[height] = selected[0]
		if capability == earliestTrace && height > 1 {
			selections.byHeight[height-1] = selected[0]
		}
		selections.mu.Unlock()
	}
	return rpctypes.BlockNumber(height), nil
}

func (b *Backend) resolveEarliestBlockOrHash(block rpctypes.BlockNumberOrHash, capability string) (rpctypes.BlockNumberOrHash, error) {
	if block.BlockNumber == nil || block.BlockHash != nil {
		return block, nil
	}
	height, err := b.ResolveEarliestBlockNumber(*block.BlockNumber, capability)
	if err != nil {
		return block, err
	}
	// Copy the value rather than mutating a selector owned by another request.
	block.BlockNumber = &height
	return block, nil
}
