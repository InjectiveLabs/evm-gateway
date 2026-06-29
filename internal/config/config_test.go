package config

import "testing"

func TestDefaultConfigEnablesParallelTipAndGapSync(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.ParallelSyncTipAndGaps {
		t.Fatal("expected parallel tip and gap sync to be enabled by default")
	}
}

func TestLoadOverridesParallelTipAndGapSync(t *testing.T) {
	t.Setenv("WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.ParallelSyncTipAndGaps {
		t.Fatal("expected parallel tip and gap sync to be disabled from env")
	}
}

// TestLoadOverridesCosmosEventVirtualization verifies the env override for
// enabling virtualized Cosmos x/bank event RPC output.
func TestLoadOverridesCosmosEventVirtualization(t *testing.T) {
	t.Setenv("WEB3INJ_VIRTUALIZE_COSMOS_EVENTS", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.VirtualizeCosmosEvents {
		t.Fatal("expected cosmos event virtualization to be enabled from env")
	}
}

func TestLoadDefaultsCometBroadcastRPCToCometRPC(t *testing.T) {
	t.Setenv("WEB3INJ_COMET_RPC", "http://sync:26657")
	t.Setenv("WEB3INJ_COMET_BROADCAST_RPC", "")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.CometBroadcastRPC != cfg.CometRPC {
		t.Fatalf("expected comet broadcast rpc to default to comet rpc: got %q want %q", cfg.CometBroadcastRPC, cfg.CometRPC)
	}
}

func TestLoadOverridesCometBroadcastRPC(t *testing.T) {
	t.Setenv("WEB3INJ_COMET_RPC", "http://sync:26657")
	t.Setenv("WEB3INJ_COMET_BROADCAST_RPC", "http://broadcast:26657")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.CometBroadcastRPC != "http://broadcast:26657" {
		t.Fatalf("unexpected comet broadcast rpc: got %q", cfg.CometBroadcastRPC)
	}
}

func TestValidateOfflineRPCOnlyRequiresChainID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableSync = false
	cfg.OfflineRPCOnly = true
	cfg.ChainID = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing chain-id validation error")
	}
}

func TestValidateOfflineRPCOnlyRejectsSync(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableSync = true
	cfg.OfflineRPCOnly = true
	cfg.ChainID = "stressinj-1337"

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected enable-sync validation error")
	}
}

func TestValidateRejectsInvalidEVMChainID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EVMChainID = "injective-1"

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid evm-chain-id validation error")
	}
}
