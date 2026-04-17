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
