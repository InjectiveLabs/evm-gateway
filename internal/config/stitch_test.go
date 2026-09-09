package config

import "testing"

func TestStitchBackendDefaultsOffAndLoadsEnvironment(t *testing.T) {
	if DefaultConfig().StitchBackend {
		t.Fatal("Stitch must be opt-in")
	}
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WEB3INJ_STITCH_BACKEND", value)
			cfg, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.StitchBackend != (value == "true") {
				t.Fatalf("environment %s ignored", value)
			}
		})
	}
}

func TestStitchBackendRejectsOfflineMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StitchBackend = true
	cfg.OfflineRPCOnly = true
	cfg.EnableSync = false
	cfg.ChainID = "injective-1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("offline Stitch configuration must be rejected")
	}
}
