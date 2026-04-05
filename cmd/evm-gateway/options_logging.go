package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initLoggingOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.logFormat = app.String(cli.StringOpt{
		Name:   "log-format",
		Desc:   "Log format: json or text.",
		EnvVar: "WEB3INJ_LOG_FORMAT",
		Value:  defaults.LogFormat,
	})

	opts.logVerbose = app.Bool(cli.BoolOpt{
		Name:   "log-verbose",
		Desc:   "Enable verbose logging.",
		EnvVar: "WEB3INJ_LOG_VERBOSE",
		Value:  defaults.LogVerbose,
	})
}
