package backend

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/cosmos/cosmos-sdk/client"
	codecTypes "github.com/cosmos/cosmos-sdk/codec/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const fixtureEarliestHeight int64 = 127000123

type earliestCall struct {
	method        string
	md            metadata.MD
	requestHeight int64
}

// A real gRPC peer keeps these tests independent of the gateway's resolver:
// headers are transported, not injected through a mocked CallOption.
type earliestPeer struct {
	evmtypes.UnimplementedQueryServer
	mu           sync.Mutex
	calls        []earliestCall
	headers      metadata.MD
	discoveryErr error
	accountErr   error
	sequence     uint64
	shard        bool
	floor, upper int64
	delay        time.Duration
}

func (p *earliestPeer) record(ctx context.Context, method string) metadata.MD {
	md, _ := metadata.FromIncomingContext(ctx)
	p.mu.Lock()
	p.calls = append(p.calls, earliestCall{method: method, md: md.Copy()})
	p.mu.Unlock()
	return md
}

func (p *earliestPeer) snapshot() []earliestCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]earliestCall(nil), p.calls...)
}

func (p *earliestPeer) Params(ctx context.Context, _ *evmtypes.QueryParamsRequest) (*evmtypes.QueryParamsResponse, error) {
	md := p.record(ctx, "Params")
	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if p.discoveryErr != nil {
		return nil, p.discoveryErr
	}
	if p.shard {
		h, _ := strconv.ParseInt(firstMD(md, "x-cosmos-block-height"), 10, 64)
		if h == 0 {
			h = p.upper
		}
		if h < p.floor || h > p.upper {
			return nil, status.Errorf(codes.Unknown, "failed to load state at height %d; version mismatch on immutable IAVL tree; version does not exist. Version has either been pruned, or is for a future block height (latest height: %d)", h, p.upper)
		}
		_ = grpc.SendHeader(ctx, metadata.Pairs("x-cosmos-block-height", strconv.FormatInt(h, 10)))
	} else {
		_ = grpc.SendHeader(ctx, p.headers)
	}
	return &evmtypes.QueryParamsResponse{Params: evmtypes.Params{EvmDenom: "inj"}}, nil
}

func (p *earliestPeer) Code(ctx context.Context, _ *evmtypes.QueryCodeRequest) (*evmtypes.QueryCodeResponse, error) {
	p.record(ctx, "Code")
	return &evmtypes.QueryCodeResponse{}, nil
}

func (p *earliestPeer) Storage(ctx context.Context, _ *evmtypes.QueryStorageRequest) (*evmtypes.QueryStorageResponse, error) {
	p.record(ctx, "Storage")
	return &evmtypes.QueryStorageResponse{Value: "0x0"}, nil
}

func (p *earliestPeer) TraceBlock(ctx context.Context, req *evmtypes.QueryTraceBlockRequest) (*evmtypes.QueryTraceBlockResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	p.mu.Lock()
	p.calls = append(p.calls, earliestCall{method: "TraceBlock", md: md.Copy(), requestHeight: req.BlockNumber})
	p.mu.Unlock()
	return &evmtypes.QueryTraceBlockResponse{Data: []byte(`[{}]`)}, nil
}

type earliestAuthPeer struct {
	authtypes.UnimplementedQueryServer
	p *earliestPeer
}

func (a earliestAuthPeer) Account(ctx context.Context, _ *authtypes.QueryAccountRequest) (*authtypes.QueryAccountResponse, error) {
	md := a.p.record(ctx, "Account")
	if a.p.shard {
		h, _ := strconv.ParseInt(firstMD(md, "x-cosmos-block-height"), 10, 64)
		if h < a.p.floor || h > a.p.upper {
			return nil, status.Error(codes.Unavailable, "wrong snapshot")
		}
		_ = grpc.SendHeader(ctx, metadata.Pairs("x-cosmos-block-height", strconv.FormatInt(h, 10)))
	}
	if a.p.accountErr != nil {
		return nil, a.p.accountErr
	}
	acc, err := codecTypes.NewAnyWithValue(&authtypes.BaseAccount{Sequence: a.p.sequence})
	return &authtypes.QueryAccountResponse{Account: acc}, err
}

func firstMD(md metadata.MD, key string) string {
	if values := md.Get(key); len(values) > 0 {
		return values[0]
	}
	return ""
}

func startEarliestPeer(t *testing.T, p *earliestPeer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	evmtypes.RegisterQueryServer(srv, p)
	authtypes.RegisterQueryServer(srv, &earliestAuthPeer{p: p})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func earliestBackend(t *testing.T, addr string, enabled bool) *Backend {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	registry := codecTypes.NewInterfaceRegistry()
	authtypes.RegisterInterfaces(registry)
	clientCtx := client.Context{}.WithGRPCClient(conn).WithInterfaceRegistry(registry)
	return &Backend{
		logger:        backendTestLogger(),
		cfg:           appconfig.Config{StitchBackend: enabled},
		clientCtx:     clientCtx,
		queryClient:   rpctypes.NewQueryClient(clientCtx),
		baseTraceTags: newBackendTraceTags(),
	}
}

func goodEarliestHeaders() metadata.MD {
	h := strconv.FormatInt(fixtureEarliestHeight, 10)
	return metadata.Pairs("x-stitch-earliest-height", h, "x-cosmos-block-height", h, "x-stitch-backend", "oldest")
}

func TestStitchEarliestValidatesDiscoveryMetadata(t *testing.T) {
	tests := []struct {
		name    string
		headers metadata.MD
		err     error
	}{
		{"missing", nil, nil},
		{"legacy upstream", metadata.Pairs("x-cosmos-block-height", "1"), nil},
		{"mismatch", metadata.Pairs("x-stitch-earliest-height", "127000123", "x-cosmos-block-height", "127000124", "x-stitch-backend", "oldest"), nil},
		{"zero", metadata.Pairs("x-stitch-earliest-height", "0", "x-cosmos-block-height", "0", "x-stitch-backend", "oldest"), nil},
		{"negative", metadata.Pairs("x-stitch-earliest-height", "-1", "x-cosmos-block-height", "-1", "x-stitch-backend", "oldest"), nil},
		{"overflow", metadata.Pairs("x-stitch-earliest-height", "9223372036854775808", "x-cosmos-block-height", "9223372036854775808", "x-stitch-backend", "oldest"), nil},
		{"duplicate height", metadata.Pairs("x-stitch-earliest-height", "127000123", "x-stitch-earliest-height", "127000123", "x-cosmos-block-height", "127000123", "x-stitch-backend", "oldest"), nil},
		{"missing backend", metadata.Pairs("x-stitch-earliest-height", "127000123", "x-cosmos-block-height", "127000123"), nil},
		{"duplicate backend", metadata.Pairs("x-stitch-earliest-height", "127000123", "x-cosmos-block-height", "127000123", "x-stitch-backend", "a", "x-stitch-backend", "b"), nil},
		{"incomplete search", nil, status.Error(codes.Unavailable, "earliest search incomplete")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			peer := &earliestPeer{headers: tc.headers, discoveryErr: tc.err}
			b := earliestBackend(t, startEarliestPeer(t, peer), true)
			nonce, err := b.GetTransactionCount(common.Address{}, rpctypes.EthEarliestBlockNumber)
			if err == nil || nonce != nil {
				t.Fatalf("invalid discovery returned nonce %v, err %v", nonce, err)
			}
			if calls := peer.snapshot(); len(calls) != 1 || calls[0].method != "Params" {
				t.Fatalf("discovery failure performed dependent reads: %+v", calls)
			}
		})
	}
}

func TestStitchEarliestStateReadsUseOneVerifiedSnapshot(t *testing.T) {
	for _, scenario := range []string{"nonce", "zero nonce", "absent account", "account unavailable", "empty code", "zero storage"} {
		t.Run(scenario, func(t *testing.T) {
			peer := &earliestPeer{headers: goodEarliestHeaders(), sequence: 7}
			switch scenario {
			case "zero nonce":
				peer.sequence = 0
			case "absent account":
				peer.accountErr = status.Error(codes.NotFound, "account not found")
			case "account unavailable":
				peer.accountErr = status.Error(codes.Unavailable, "backend unavailable")
			}
			b := earliestBackend(t, startEarliestPeer(t, peer), true)
			num := rpctypes.EthEarliestBlockNumber
			selector := rpctypes.BlockNumberOrHash{BlockNumber: &num}
			switch scenario {
			case "empty code":
				code, err := b.GetCode(common.Address{}, selector)
				if err != nil || len(code) != 0 {
					t.Fatalf("empty code: %x, %v", code, err)
				}
			case "zero storage":
				value, err := b.GetStorageAt(common.Address{}, "0x0", selector)
				if err != nil || common.BytesToHash(value) != (common.Hash{}) {
					t.Fatalf("zero storage: %x, %v", value, err)
				}
			default:
				nonce, err := b.GetTransactionCount(common.Address{}, num)
				if scenario == "account unavailable" {
					if err == nil || nonce != nil {
						t.Fatalf("transport failure became nonce: %v %v", nonce, err)
					}
				} else {
					want := peer.sequence
					if scenario == "absent account" {
						want = 0
					}
					if err != nil || nonce == nil || uint64(*nonce) != want {
						t.Fatalf("nonce=%v error=%v want=%d", nonce, err, want)
					}
				}
			}
			calls := peer.snapshot()
			if len(calls) != 2 {
				t.Fatalf("want discovery and one dependent query, got %+v", calls)
			}
			md := calls[0].md
			if calls[0].method != "Params" || firstMD(md, "stitch") != "earliest" || firstMD(md, "x-stitch-earliest-capability") != "state" || firstMD(md, "x-cosmos-block-height") != "1" {
				t.Fatalf("wrong discovery metadata: %+v", calls[0])
			}
			md = calls[1].md
			if firstMD(md, "x-cosmos-block-height") != "127000123" || firstMD(md, "x-stitch-backend") != "oldest" || len(md.Get("stitch")) != 0 || len(md.Get("x-stitch-earliest-capability")) != 0 {
				t.Fatalf("wrong dependent metadata: %+v", calls[1])
			}
			if num != rpctypes.EthEarliestBlockNumber {
				t.Fatal("mutated caller's selector")
			}
		})
	}
}

func TestStitchEarliestDoesNotRewriteOtherSelectors(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, height := range []rpctypes.BlockNumber{rpctypes.EthEarliestBlockNumber, 1, 127250000, rpctypes.EthLatestBlockNumber, rpctypes.EthPendingBlockNumber} {
			if enabled && height == rpctypes.EthEarliestBlockNumber {
				continue
			}
			t.Run(fmt.Sprintf("enabled_%v_height_%d", enabled, height), func(t *testing.T) {
				peer := &earliestPeer{}
				b := earliestBackend(t, startEarliestPeer(t, peer), enabled)
				_, err := b.GetCode(common.Address{}, rpctypes.BlockNumberOrHash{BlockNumber: &height})
				if err != nil {
					t.Fatal(err)
				}
				calls := peer.snapshot()
				if len(calls) != 1 || calls[0].method != "Code" {
					t.Fatalf("unexpected discovery: %+v", calls)
				}
				if len(calls[0].md.Get("stitch")) != 0 || len(calls[0].md.Get("x-stitch-backend")) != 0 {
					t.Fatalf("routing markers leaked: %+v", calls[0])
				}
				want := strconv.FormatInt(height.Int64(), 10)
				if height.Int64() == 0 {
					want = ""
				}
				if firstMD(calls[0].md, "x-cosmos-block-height") != want {
					t.Fatalf("height changed: %+v want %s", calls[0], want)
				}
			})
		}
	}
}

func TestStitchEarliestCapabilityAndConcurrentRequestIsolation(t *testing.T) {
	peer := &earliestPeer{headers: goodEarliestHeaders()}
	b := earliestBackend(t, startEarliestPeer(t, peer), true)
	var wg sync.WaitGroup
	for _, capability := range []string{"state", "block", "execution", "trace", "proof", "storage", "range"} {
		wg.Add(1)
		go func(capability string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			op := b.WithContext(ctx).(*Backend)
			h, err := op.ResolveEarliestBlockNumber(rpctypes.EthEarliestBlockNumber, capability)
			if err != nil || int64(h) != fixtureEarliestHeight {
				t.Errorf("resolve %s: %d %v", capability, h, err)
				return
			}
			if earliestBackendAt(op.contextWithHeight(int64(h)), int64(h)) != "oldest" {
				t.Error("selection missing from dependent context")
			}
			for _, other := range []int64{0, fixtureEarliestHeight + 1} {
				md, _ := metadata.FromOutgoingContext(op.contextWithHeight(other))
				if len(md.Get("x-stitch-backend")) != 0 {
					t.Errorf("selection leaked to %d: %v", other, md)
				}
			}
			md, _ := metadata.FromOutgoingContext(op.contextWithHeight(fixtureEarliestHeight - 1))
			if got := firstMD(md, "x-stitch-backend"); (capability == "trace") != (got == "oldest") {
				t.Errorf("capability %s parent state selection %q", capability, got)
			}
		}(capability)
	}
	wg.Wait()
	for _, call := range peer.snapshot() {
		if firstMD(call.md, "stitch") != "earliest" || firstMD(call.md, "x-cosmos-block-height") != "1" || !strings.Contains("state block execution trace proof storage range", firstMD(call.md, "x-stitch-earliest-capability")) {
			t.Errorf("invalid discovery: %+v", call)
		}
	}
	if got := earliestBackendAt(b.operationContext(), fixtureEarliestHeight); got != "" {
		t.Fatalf("request selection leaked to shared backend: %s", got)
	}
	other := b.WithContext(context.Background()).(*Backend)
	if got := earliestBackendAt(other.operationContext(), fixtureEarliestHeight); got != "" {
		t.Fatalf("request selection leaked to next request: %s", got)
	}
}
