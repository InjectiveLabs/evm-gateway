package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultTraceTimeoutCap(t *testing.T) {
	if got := DefaultConfig().JSONRPC.TraceTimeoutCap; got != 30*time.Second {
		t.Fatalf("default trace timeout cap = %s, want 30s", got)
	}
}

func TestLoadTraceTimeoutCapFromEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: 30 * time.Second},
		{name: "explicit default", value: "30s", want: 30 * time.Second},
		{name: "lower", value: "5s", want: 5 * time.Second},
		{name: "higher", value: "60s", want: 60 * time.Second},
		{name: "malformed", value: "invalid", wantErr: true},
		{name: "overflow", value: "999999999999999999999s", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-1s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("WEB3INJ_JSONRPC_TRACE_TIMEOUT_CAP", tc.value)
			cfg, err := Load("")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "trace-timeout-cap") {
					t.Fatalf("Load(%q) error = %v, want trace timeout cap error", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.JSONRPC.TraceTimeoutCap != tc.want {
				t.Fatalf("trace timeout cap = %s, want %s", cfg.JSONRPC.TraceTimeoutCap, tc.want)
			}
		})
	}
}

func TestLoadTraceTimeoutCapFromEnvFile(t *testing.T) {
	t.Setenv("WEB3INJ_JSONRPC_TRACE_TIMEOUT_CAP", "")
	envFile := filepath.Join(t.TempDir(), "gateway.env")
	if err := os.WriteFile(envFile, []byte("WEB3INJ_JSONRPC_TRACE_TIMEOUT_CAP=5s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(envFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JSONRPC.TraceTimeoutCap != 5*time.Second {
		t.Fatalf("trace timeout cap = %s, want 5s", cfg.JSONRPC.TraceTimeoutCap)
	}
}

func TestValidateTraceTimeoutCap(t *testing.T) {
	for _, value := range []time.Duration{0, -time.Second} {
		cfg := DefaultConfig()
		cfg.JSONRPC.TraceTimeoutCap = value
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "trace-timeout-cap must be positive") {
			t.Fatalf("cap %s validation error = %v, want positive cap error", value, err)
		}
	}
}
