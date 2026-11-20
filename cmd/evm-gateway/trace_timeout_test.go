package main

import (
	"strings"
	"testing"
	"time"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func TestCLITraceTimeoutCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		args    []string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: 30 * time.Second},
		{name: "explicit default", env: "30s", want: 30 * time.Second},
		{name: "lower", env: "5s", want: 5 * time.Second},
		{name: "higher start command", env: "60s", args: []string{"start"}, want: 60 * time.Second},
		{name: "flag overrides environment", env: "5s", args: []string{"--rpc-trace-timeout-cap=60s"}, want: 60 * time.Second},
		{name: "malformed", env: "invalid", wantErr: true},
		{name: "overflow", env: "999999999999999999999s", wantErr: true},
		{name: "zero", env: "0s", wantErr: true},
		{name: "negative", env: "-1s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("WEB3INJ_JSONRPC_TRACE_TIMEOUT_CAP", tc.env)
			previousRunner := startRunner
			t.Cleanup(func() { startRunner = previousRunner })
			var got config.Config
			var configErr error
			called := false
			startRunner = func(opts *gatewayCLIOptions) error {
				called = true
				got, configErr = buildConfig(opts)
				// Capture validation without starting the app or exiting this test process.
				return nil
			}
			if err := newGatewayCLI().Run(append([]string{"evm-gateway"}, tc.args...)); err != nil {
				t.Fatalf("CLI parsing: %v", err)
			}
			if !called {
				t.Fatal("CLI did not invoke the start configuration path")
			}
			if tc.wantErr {
				if configErr == nil || !strings.Contains(configErr.Error(), "trace-timeout-cap") {
					t.Fatalf("buildConfig error = %v, want trace timeout cap error", configErr)
				}
				return
			}
			if configErr != nil {
				t.Fatalf("buildConfig: %v", configErr)
			}
			if got.JSONRPC.TraceTimeoutCap != tc.want {
				t.Fatalf("CLI trace timeout cap = %s, want %s", got.JSONRPC.TraceTimeoutCap, tc.want)
			}
		})
	}
}
