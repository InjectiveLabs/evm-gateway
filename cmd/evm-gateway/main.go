package main

import (
	"log"
	"os"

	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func main() {
	if err := newGatewayCLI().Run(os.Args); err != nil {
		log.Fatalln(err)
	}
}

func newGatewayCLI() *cli.Cli {
	readEnv()
	defaults := config.DefaultConfig()

	app := cli.App(
		"evm-gateway",
		"Standalone Ethereum JSON-RPC gateway for Injective EVM. Runs `start` when no action is specified.",
	)

	opts := &gatewayCLIOptions{}
	initGlobalOptions(app, opts)
	initChainOptions(app, opts, defaults)
	initIndexerOptions(app, opts, defaults)
	initLoggingOptions(app, opts, defaults)
	initRPCOptions(app, opts, defaults)
	initTelemetryOptions(app, opts, defaults)

	app.Action = func() {
		runDefaultAction(opts)
	}

	app.Command("start", "Start the EVM gateway service.", func(cmd *cli.Cmd) {
		cmd.Action = func() {
			runDefaultAction(opts)
		}
	})

	app.Command("version", "Print build version information and exit.", versionCmd)
	app.Command("resync", "Resync gateway state (stub; not implemented yet).", func(cmd *cli.Cmd) {
		cmd.Action = func() {
			runOrFail(resyncRunner(opts))
		}
	})

	return app
}

func runOrFail(err error) {
	if err != nil {
		fail(err)
	}
}

func runDefaultAction(opts *gatewayCLIOptions) {
	if *opts.printVersion {
		printVersion(os.Stdout)
		return
	}

	runOrFail(startRunner(opts))
}
