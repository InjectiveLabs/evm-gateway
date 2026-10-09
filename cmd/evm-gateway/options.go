package main

import cli "github.com/jawher/mow.cli"

type gatewayCLIOptions struct {
	envFile      *string
	printVersion *bool

	chainID                *string
	evmChainID             *string
	cometRPC               *string
	cometBroadcastRPC      *string
	grpcAddr               *string
	earliest               *int
	fetchJobs              *int
	dataDir                *string
	enableSync             *bool
	parallelSyncTipAndGaps *bool
	virtualizeCosmosEvents *bool
	offlineRPCOnly         *bool

	logFormat  *string
	logVerbose *bool

	enableRPC *bool
	rpcAddr   *string
	wsAddr    *string
	rpcAPI    *string

	tracingEnabled                    *bool
	tracingDSN                        *string
	tracingCollectorAuthorization     *string
	tracingCollectorAuthorizationName *string
	tracingCollectorEnableTLS         *bool
}

func initGlobalOptions(app *cli.Cli, opts *gatewayCLIOptions) {
	opts.envFile = app.String(cli.StringOpt{
		Name: envFileFlag,
		Desc: "Path to .env file with WEB3INJ_ variables (default: .env in the working directory, if present). Must precede the command.",
	})

	opts.printVersion = app.Bool(cli.BoolOpt{
		Name: "version",
		Desc: "Print build version information and exit.",
	})
}
