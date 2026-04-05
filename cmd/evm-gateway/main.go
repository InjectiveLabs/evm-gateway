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
		bannerStr+"\nStandalone Ethereum JSON-RPC gateway for Injective EVM.",
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

	app.Command("start", "Start the EVM gateway service (runs by default).", func(cmd *cli.Cmd) {
		cmd.Action = func() {
			runDefaultAction(opts)
		}
	})

	app.Command("version", "Print build version information and exit.", versionCmd)
	app.Command("resync", "Resync gateway state for specified block range.", func(cmd *cli.Cmd) {
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

const bannerStr = `░█▀▀░█░█░█▄█░░░░░░░░░░░░░░░░
░█▀▀░▀▄▀░█░█░░░░░░░░░░░░░░░░
░▀▀▀░░▀░░▀░▀░░░░░░░░░░░░░░░░
░█▀▀░█▀█░▀█▀░█▀▀░█░█░█▀█░█░█
░█░█░█▀█░░█░░█▀▀░█▄█░█▀█░░█░
░▀▀▀░▀░▀░░▀░░▀▀▀░▀░▀░▀░▀░░▀░
`
