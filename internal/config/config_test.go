package config

import "testing"

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
