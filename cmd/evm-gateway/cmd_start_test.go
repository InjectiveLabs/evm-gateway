package main

import (
	"testing"
)

func TestBuildConfigCarriesSyncAndOfflineFlags(t *testing.T) {
	opts := &gatewayCLIOptions{
		chainID:                           stringPtr("stressinj-1337"),
		evmChainID:                        stringPtr("1337"),
		cometRPC:                          stringPtr("http://localhost:26657"),
		cometBroadcastRPC:                 stringPtr(""),
		grpcAddr:                          stringPtr("127.0.0.1:9900"),
		earliest:                          intPtr(123),
		fetchJobs:                         intPtr(4),
		dataDir:                           stringPtr(t.TempDir()),
		enableSync:                        boolPtr(false),
		parallelSyncTipAndGaps:            boolPtr(false),
		virtualizeCosmosEvents:            boolPtr(true),
		offlineRPCOnly:                    boolPtr(true),
		logFormat:                         stringPtr("json"),
		logVerbose:                        boolPtr(false),
		enableRPC:                         boolPtr(true),
		rpcAddr:                           stringPtr("127.0.0.1:8545"),
		wsAddr:                            stringPtr("127.0.0.1:8546"),
		rpcAPI:                            stringPtr("eth"),
		tracingEnabled:                    boolPtr(false),
		tracingDSN:                        stringPtr(""),
		tracingCollectorAuthorization:     stringPtr(""),
		tracingCollectorAuthorizationName: stringPtr(""),
		tracingCollectorEnableTLS:         boolPtr(true),
	}

	cfg, err := buildConfig(opts)
	if err != nil {
		t.Fatalf("buildConfig returned error: %v", err)
	}
	if cfg.EnableSync {
		t.Fatal("expected enable sync to be false")
	}
	if cfg.ParallelSyncTipAndGaps {
		t.Fatal("expected parallel tip and gap sync to be false")
	}
	if !cfg.VirtualizeCosmosEvents {
		t.Fatal("expected cosmos event virtualization to be true")
	}
	if !cfg.OfflineRPCOnly {
		t.Fatal("expected offline rpc only to be true")
	}
	if !cfg.JSONRPC.Enable {
		t.Fatal("expected jsonrpc enable to be true")
	}
	if cfg.CometBroadcastRPC != cfg.CometRPC {
		t.Fatalf("expected comet broadcast rpc to default to comet rpc: got %q want %q", cfg.CometBroadcastRPC, cfg.CometRPC)
	}
}

func TestBuildConfigOverridesCometBroadcastRPC(t *testing.T) {
	opts := &gatewayCLIOptions{
		chainID:                           stringPtr("stressinj-1337"),
		evmChainID:                        stringPtr("1337"),
		cometRPC:                          stringPtr("http://sync:26657"),
		cometBroadcastRPC:                 stringPtr("http://broadcast:26657"),
		grpcAddr:                          stringPtr("127.0.0.1:9900"),
		earliest:                          intPtr(123),
		fetchJobs:                         intPtr(4),
		dataDir:                           stringPtr(t.TempDir()),
		enableSync:                        boolPtr(false),
		parallelSyncTipAndGaps:            boolPtr(false),
		virtualizeCosmosEvents:            boolPtr(false),
		offlineRPCOnly:                    boolPtr(true),
		logFormat:                         stringPtr("json"),
		logVerbose:                        boolPtr(false),
		enableRPC:                         boolPtr(true),
		rpcAddr:                           stringPtr("127.0.0.1:8545"),
		wsAddr:                            stringPtr("127.0.0.1:8546"),
		rpcAPI:                            stringPtr("eth"),
		tracingEnabled:                    boolPtr(false),
		tracingDSN:                        stringPtr(""),
		tracingCollectorAuthorization:     stringPtr(""),
		tracingCollectorAuthorizationName: stringPtr(""),
		tracingCollectorEnableTLS:         boolPtr(true),
	}

	cfg, err := buildConfig(opts)
	if err != nil {
		t.Fatalf("buildConfig returned error: %v", err)
	}
	if cfg.CometBroadcastRPC != "http://broadcast:26657" {
		t.Fatalf("unexpected comet broadcast rpc: got %q", cfg.CometBroadcastRPC)
	}
}

func stringPtr(v string) *string { return &v }
func intPtr(v int) *int          { return &v }
func boolPtr(v bool) *bool       { return &v }
