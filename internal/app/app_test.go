package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func TestBuildClientContextOfflineRPCOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnableSync = false
	cfg.OfflineRPCOnly = true
	cfg.ChainID = "stressinj-1337"

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
