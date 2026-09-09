package main

import (
	"testing"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	cli "github.com/jawher/mow.cli"
)

func TestStitchBackendCLIAndEnvironmentReachRuntimeConfig(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		want      bool
	}{
		{"environment", "true", nil, true},
		{"CLI enables", "false", []string{"--stitch-backend"}, true},
		{"CLI disables", "true", []string{"--stitch-backend=false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEB3INJ_STITCH_BACKEND", tc.env)
			app := cli.App("fixture", "test option mapping")
			opts := &gatewayCLIOptions{}
			defaults := config.DefaultConfig()
			initChainOptions(app, opts, defaults)
			initIndexerOptions(app, opts, defaults)
			initLoggingOptions(app, opts, defaults)
			initRPCOptions(app, opts, defaults)
			initTelemetryOptions(app, opts, defaults)
			called := false
			app.Action = func() {
				called = true
				cfg, err := buildConfig(opts)
				if err != nil {
					t.Error(err)
					return
				}
				if cfg.StitchBackend != tc.want {
					t.Errorf("runtime StitchBackend=%v, want %v", cfg.StitchBackend, tc.want)
				}
			}
			if err := app.Run(append([]string{"fixture"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("runtime configuration not built")
			}
		})
	}
}
