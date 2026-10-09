package main

import (
	"strings"
	"testing"

	gatewayapp "github.com/InjectiveLabs/evm-gateway/internal/app"
)

func migrateTestCLIOptions(t *testing.T) *gatewayCLIOptions {
	t.Helper()
	return &gatewayCLIOptions{
		chainID:                           stringPtr("injective-1"),
		evmChainID:                        stringPtr("1776"),
		cometRPC:                          stringPtr("http://127.0.0.1:1"),
		cometBroadcastRPC:                 stringPtr(""),
		grpcAddr:                          stringPtr("127.0.0.1:1"),
		earliest:                          intPtr(0),
		fetchJobs:                         intPtr(2),
		dataDir:                           stringPtr(t.TempDir()),
		enableSync:                        boolPtr(false),
		parallelSyncTipAndGaps:            boolPtr(false),
		virtualizeCosmosEvents:            boolPtr(false),
		offlineRPCOnly:                    boolPtr(false),
		logFormat:                         stringPtr("text"),
		logVerbose:                        boolPtr(false),
		enableRPC:                         boolPtr(false),
		rpcAddr:                           stringPtr("127.0.0.1:0"),
		wsAddr:                            stringPtr("127.0.0.1:0"),
		rpcAPI:                            stringPtr("eth"),
		tracingEnabled:                    boolPtr(false),
		tracingDSN:                        stringPtr(""),
		tracingCollectorAuthorization:     stringPtr(""),
		tracingCollectorAuthorizationName: stringPtr(""),
		tracingCollectorEnableTLS:         boolPtr(true),
	}
}

func TestMigrateAnteFailedCommandParsesOptions(t *testing.T) {
	original := migrateAnteFailedRunner
	defer func() { migrateAnteFailedRunner = original }()

	testCases := []struct {
		name string
		args []string
		want gatewayapp.MigrateAnteFailedOptions
	}{
		{
			name: "defaults",
			args: []string{"migrate", "ante-failed-txs"},
			want: gatewayapp.MigrateAnteFailedOptions{},
		},
		{
			name: "all flags",
			args: []string{"migrate", "ante-failed-txs", "--from", "185577968", "--to", "185579953", "--scan-only", "--dry-run", "--force", "--report", "/tmp/report.json"},
			want: gatewayapp.MigrateAnteFailedOptions{
				From:       185577968,
				To:         185579953,
				ScanOnly:   true,
				DryRun:     true,
				Force:      true,
				ReportPath: "/tmp/report.json",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				called bool
				got    gatewayapp.MigrateAnteFailedOptions
			)
			migrateAnteFailedRunner = func(opts *gatewayCLIOptions, migrateOpts gatewayapp.MigrateAnteFailedOptions) error {
				if opts == nil {
					t.Fatalf("expected global options")
				}
				called = true
				got = migrateOpts
				return nil
			}
			if err := newGatewayCLI().Run(append([]string{"evm-gateway"}, tc.args...)); err != nil {
				t.Fatalf("cli run: %v", err)
			}
			if !called {
				t.Fatalf("migration runner not called")
			}
			if got != tc.want {
				t.Fatalf("unexpected options: got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestRunMigrateAnteFailed(t *testing.T) {
	t.Run("scan-only on empty state", func(t *testing.T) {
		if err := runMigrateAnteFailed(migrateTestCLIOptions(t), gatewayapp.MigrateAnteFailedOptions{ScanOnly: true}); err != nil {
			t.Fatalf("scan-only migration: %v", err)
		}
	})

	t.Run("invalid range", func(t *testing.T) {
		err := runMigrateAnteFailed(migrateTestCLIOptions(t), gatewayapp.MigrateAnteFailedOptions{From: 10, To: 5})
		if err == nil || !strings.Contains(err.Error(), "invalid height range") {
			t.Fatalf("expected invalid range error, got %v", err)
		}
	})

}
