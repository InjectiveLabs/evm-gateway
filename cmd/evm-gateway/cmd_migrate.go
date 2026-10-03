package main

import (
	"os"

	cli "github.com/jawher/mow.cli"

	gatewayapp "github.com/InjectiveLabs/evm-gateway/internal/app"
	"github.com/InjectiveLabs/evm-gateway/internal/logging"
)

var migrateAnteFailedRunner = runMigrateAnteFailed

type migrateAnteFailedCLIOptions struct {
	from     *int
	to       *int
	scanOnly *bool
	dryRun   *bool
	force    *bool
	report   *string
}

func initMigrateAnteFailedOptions(cmd *cli.Cmd) *migrateAnteFailedCLIOptions {
	return &migrateAnteFailedCLIOptions{
		from: cmd.Int(cli.IntOpt{
			Name:  "from",
			Desc:  "Lowest block height to scan (0 = earliest indexed).",
			Value: 0,
		}),
		to: cmd.Int(cli.IntOpt{
			Name:  "to",
			Desc:  "Highest block height to scan (0 = latest indexed).",
			Value: 0,
		}),
		scanOnly: cmd.Bool(cli.BoolOpt{
			Name:  "scan-only",
			Desc:  "Only scan local state offline and report candidates; no network access, no writes.",
			Value: false,
		}),
		dryRun: cmd.Bool(cli.BoolOpt{
			Name:  "dry-run",
			Desc:  "Verify candidates against the node and report the heights to resync without writing.",
			Value: false,
		}),
		force: cmd.Bool(cli.BoolOpt{
			Name:  "force",
			Desc:  "Run again even if the migration was already completed.",
			Value: false,
		}),
		report: cmd.String(cli.StringOpt{
			Name:  "report",
			Desc:  "Write a JSON report of the scan, verification and repair to this path.",
			Value: "",
		}),
	}
}

func (o *migrateAnteFailedCLIOptions) toOptions() gatewayapp.MigrateAnteFailedOptions {
	return gatewayapp.MigrateAnteFailedOptions{
		From:       int64(*o.from),
		To:         int64(*o.to),
		ScanOnly:   *o.scanOnly,
		DryRun:     *o.dryRun,
		Force:      *o.force,
		ReportPath: *o.report,
	}
}

func runMigrateAnteFailed(opts *gatewayCLIOptions, migrateOpts gatewayapp.MigrateAnteFailedOptions) error {
	cfg, err := buildConfig(opts)
	if err != nil {
		return err
	}

	logger := logging.New(logging.Config{
		Format:  cfg.LogFormat,
		Verbose: cfg.LogVerbose,
		Output:  os.Stdout,
	})

	initSDKConfig()

	if err := gatewayapp.RunMigrateAnteFailed(cfg, logger, migrateOpts); err != nil {
		logger.Error("migration failed", "error", err)
		return err
	}
	return nil
}
