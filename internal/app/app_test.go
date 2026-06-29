package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	sdkmath "cosmossdk.io/math"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/cosmos/cosmos-sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func TestBuildClientContextOfflineRPCOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnableSync = false
	cfg.OfflineRPCOnly = true
	cfg.ChainID = "stressinj-1337"
	cfg.EVMChainID = "1337"

	clientCtx, rpcClient, grpcConn, err := buildClientContext(
		context.Background(),
		&cfg,
		t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("buildClientContext returned error: %v", err)
	}
	if rpcClient != nil {
		t.Fatalf("expected nil rpc client, got %T", rpcClient)
	}
	if grpcConn != nil {
		t.Fatalf("expected nil grpc connection, got %T", grpcConn)
	}
	if clientCtx.ChainID != cfg.ChainID {
		t.Fatalf("unexpected chain id: got %q want %q", clientCtx.ChainID, cfg.ChainID)
	}
}

func TestBuildClientContextOfflineRPCOnlyDerivesEVMChainID(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnableSync = false
	cfg.OfflineRPCOnly = true
	cfg.ChainID = "stressinj-1337"

	_, _, _, err := buildClientContext(
		context.Background(),
		&cfg,
		t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("buildClientContext returned error: %v", err)
	}
	if cfg.EVMChainID != "1337" {
		t.Fatalf("unexpected evm chain id: got %q want 1337", cfg.EVMChainID)
	}
}

func TestBuildBroadcastClientContextDefaultsToPrimaryCometClient(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CometRPC = "http://sync:26657"
	cfg.CometBroadcastRPC = ""

	clientCtx := client.Context{}.WithNodeURI(cfg.CometRPC)
	broadcastCtx, rpcClient, err := buildBroadcastClientContext(
		&cfg,
		clientCtx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("buildBroadcastClientContext returned error: %v", err)
	}
	if rpcClient != nil {
		t.Fatalf("expected no separate broadcast client, got %T", rpcClient)
	}
	if broadcastCtx.NodeURI != cfg.CometRPC {
		t.Fatalf("unexpected broadcast node URI: got %q want %q", broadcastCtx.NodeURI, cfg.CometRPC)
	}
	if cfg.CometBroadcastRPC != cfg.CometRPC {
		t.Fatalf("expected config broadcast rpc to normalize to comet rpc: got %q want %q", cfg.CometBroadcastRPC, cfg.CometRPC)
	}
}

func TestBuildBroadcastClientContextUsesSeparateCometClient(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CometRPC = "http://sync:26657"
	cfg.CometBroadcastRPC = "http://broadcast:26657"

	broadcastCtx, rpcClient, err := buildBroadcastClientContext(
		&cfg,
		client.Context{}.WithNodeURI(cfg.CometRPC),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("buildBroadcastClientContext returned error: %v", err)
	}
	if rpcClient == nil {
		t.Fatal("expected separate broadcast rpc client")
	}
	t.Cleanup(func() { _ = rpcClient.Stop() })
	if broadcastCtx.NodeURI != cfg.CometBroadcastRPC {
		t.Fatalf("unexpected broadcast node URI: got %q want %q", broadcastCtx.NodeURI, cfg.CometBroadcastRPC)
	}
	if broadcastCtx.Client == nil {
		t.Fatal("expected broadcast context to have a comet client")
	}
}

type stubEVMParamsClient struct {
	resp *evmtypes.QueryParamsResponse
	err  error
}

func (c stubEVMParamsClient) Params(context.Context, *evmtypes.QueryParamsRequest, ...grpc.CallOption) (*evmtypes.QueryParamsResponse, error) {
	return c.resp, c.err
}

func TestFetchEVMChainID(t *testing.T) {
	chainID := sdkmath.NewInt(1776)
	got, err := fetchEVMChainID(context.Background(), stubEVMParamsClient{
		resp: &evmtypes.QueryParamsResponse{
			Params: evmtypes.Params{
				ChainConfig: evmtypes.ChainConfig{EIP155ChainID: &chainID},
			},
		},
	})
	if err != nil {
		t.Fatalf("fetchEVMChainID returned error: %v", err)
	}
	if got != "1776" {
		t.Fatalf("unexpected evm chain id: got %q want 1776", got)
	}
}

func TestResolveEVMChainIDUsesDefaultForVirtualizedBackfillWhenEVMParamsUnavailable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.VirtualizeCosmosEvents = true

	err := resolveEVMChainID(
		context.Background(),
		&cfg,
		stubEVMParamsClient{err: grpcstatus.Error(codes.Unimplemented, "unknown service injective.evm.v1.Query")},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("resolveEVMChainID returned error: %v", err)
	}
	if cfg.EVMChainID != "1776" {
		t.Fatalf("unexpected fallback evm chain id: got %q want 1776", cfg.EVMChainID)
	}
}

func TestResolveEVMChainIDUsesConfiguredIDForVirtualizedBackfillWhenEVMParamsUnavailable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.VirtualizeCosmosEvents = true
	cfg.EVMChainID = "999"

	err := resolveEVMChainID(
		context.Background(),
		&cfg,
		stubEVMParamsClient{err: grpcstatus.Error(codes.Unimplemented, "unknown service injective.evm.v1.Query")},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("resolveEVMChainID returned error: %v", err)
	}
	if cfg.EVMChainID != "999" {
		t.Fatalf("unexpected evm chain id: got %q want 999", cfg.EVMChainID)
	}
}

func TestResolveEVMChainIDRequiresEVMParamsOutsideVirtualizedBackfill(t *testing.T) {
	cfg := config.DefaultConfig()

	err := resolveEVMChainID(
		context.Background(),
		&cfg,
		stubEVMParamsClient{err: grpcstatus.Error(codes.Unimplemented, "unknown service injective.evm.v1.Query")},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err == nil {
		t.Fatal("expected missing evm params to fail when virtualized cosmos events are disabled")
	}
}

func TestProtocolAndAddressAddsDefaultPorts(t *testing.T) {
	tcpProto, tcpAddr := protocolAndAddress("localhost:9090")
	if tcpProto != "tcp" || tcpAddr != "localhost:9090" {
		t.Fatalf("unexpected tcp normalization: %q %q", tcpProto, tcpAddr)
	}

	httpsProto, httpsAddr := protocolAndAddress("https://evm.archival.chain.grpc.injective.network")
	if httpsProto != "https" || httpsAddr != "evm.archival.chain.grpc.injective.network:443" {
		t.Fatalf("unexpected https normalization: %q %q", httpsProto, httpsAddr)
	}

	httpProto, httpAddr := protocolAndAddress("http://example.com")
	if httpProto != "http" || httpAddr != "example.com:80" {
		t.Fatalf("unexpected http normalization: %q %q", httpProto, httpAddr)
	}
}
