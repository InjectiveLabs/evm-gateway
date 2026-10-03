package main

import (
	"os"
	"path/filepath"
	"testing"

	gatewayapp "github.com/InjectiveLabs/evm-gateway/internal/app"
)

func TestEnvFileFromArgs(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no args", args: nil, want: ""},
		{name: "binary only", args: []string{"evm-gateway"}, want: ""},
		{name: "separate value", args: []string{"evm-gateway", "--env-file", "a.env", "start"}, want: "a.env"},
		{name: "equals value", args: []string{"evm-gateway", "--env-file=b.env", "resync", "1"}, want: "b.env"},
		{name: "single dash", args: []string{"evm-gateway", "-env-file=c.env"}, want: "c.env"},
		{name: "after other options", args: []string{"evm-gateway", "--log-verbose", "--data-dir", "/d", "--env-file", "d.env"}, want: "d.env"},
		{name: "missing value", args: []string{"evm-gateway", "--env-file"}, want: ""},
		{name: "after command", args: []string{"evm-gateway", "start", "--env-file", "e.env"}, want: "e.env"},
		{name: "after terminator", args: []string{"evm-gateway", "--", "--env-file", "f.env"}, want: ""},
		{name: "similar name", args: []string{"evm-gateway", "--env-file-x", "g.env"}, want: ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envFileFromArgs(tc.args); got != tc.want {
				t.Fatalf("envFileFromArgs(%q) = %q want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestLoadEnvFileFlagFeedsOptionDefaults(t *testing.T) {
	t.Setenv("WEB3INJ_COMET_RPC", "http://from-process-env:26657")
	t.Setenv("WEB3INJ_FETCH_JOBS", "")
	envFile := filepath.Join(t.TempDir(), "custom.env")
	content := "# comment\nWEB3INJ_COMET_RPC=\"http://from-env-file:26657\"\n WEB3INJ_FETCH_JOBS = 7 \nnot-an-assignment\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	args := []string{"evm-gateway", "--env-file", envFile, "migrate", "ante-failed-txs", "--scan-only"}
	if err := loadEnv(args); err != nil {
		t.Fatalf("loadEnv: %v", err)
	}

	original := migrateAnteFailedRunner
	defer func() { migrateAnteFailedRunner = original }()
	var gotOpts *gatewayCLIOptions
	migrateAnteFailedRunner = func(opts *gatewayCLIOptions, _ gatewayapp.MigrateAnteFailedOptions) error {
		gotOpts = opts
		return nil
	}
	if err := newGatewayCLI().Run(args); err != nil {
		t.Fatalf("cli run: %v", err)
	}
	if gotOpts == nil {
		t.Fatalf("migration runner not called")
	}
	if *gotOpts.cometRPC != "http://from-env-file:26657" {
		t.Fatalf("comet rpc not taken from env file: %q", *gotOpts.cometRPC)
	}
	if *gotOpts.fetchJobs != 7 {
		t.Fatalf("fetch jobs not taken from env file: %d", *gotOpts.fetchJobs)
	}
	if *gotOpts.envFile != envFile {
		t.Fatalf("env-file option not parsed: %q", *gotOpts.envFile)
	}
}

func TestLoadEnvMissingExplicitFileFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	if err := loadEnv([]string{"evm-gateway", "--env-file", missing}); err == nil {
		t.Fatalf("expected error for missing explicit env file")
	}
}

func TestLoadEnvDefaultDotEnvIsOptional(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := loadEnv([]string{"evm-gateway", "start"}); err != nil {
		t.Fatalf("missing default .env must be ignored: %v", err)
	}

	t.Setenv("WEB3INJ_GRPC_ADDR", "")
	if err := os.WriteFile(".env", []byte("WEB3INJ_GRPC_ADDR=grpc-from-dotenv:9090\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := loadEnv([]string{"evm-gateway"}); err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if got := os.Getenv("WEB3INJ_GRPC_ADDR"); got != "grpc-from-dotenv:9090" {
		t.Fatalf("default .env not loaded: %q", got)
	}
}
