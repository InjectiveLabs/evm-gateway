package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initIndexerOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.earliest = app.Int(cli.IntOpt{
		Name:   "earliest-block",
		Desc:   "Earliest block height to index from.",
		EnvVar: "WEB3INJ_EARLIEST_BLOCK",
		Value:  int(defaults.Earliest),
	})

	opts.fetchJobs = app.Int(cli.IntOpt{
		Name:   "fetch-jobs",
		Desc:   "Parallel block fetch jobs.",
		EnvVar: "WEB3INJ_FETCH_JOBS",
		Value:  defaults.FetchJobs,
	})

	opts.dataDir = app.String(cli.StringOpt{
		Name:   "data-dir",
		Desc:   "Data directory for indexer DB.",
		EnvVar: "WEB3INJ_DATA_DIR",
		Value:  defaults.DataDir,
	})

	opts.enableSync = app.Bool(cli.BoolOpt{
		Name:   "enable-sync",
		Desc:   "Enable indexer sync loop.",
		EnvVar: "WEB3INJ_ENABLE_SYNC",
		Value:  defaults.EnableSync,
	})

	opts.offlineRPCOnly = app.Bool(cli.BoolOpt{
		Name:   "offline-rpc-only",
		Desc:   "Start JSON-RPC with no live comet/grpc clients; requires --enable-sync=false and a chain ID.",
		EnvVar: "WEB3INJ_OFFLINE_RPC_ONLY",
		Value:  defaults.OfflineRPCOnly,
	})
}
