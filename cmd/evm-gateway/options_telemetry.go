package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initTelemetryOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.tracingEnabled = app.Bool(cli.BoolOpt{
		Name:   "gotracer-enabled",
		Desc:   "Enable gotracer.",
		EnvVar: "WEB3INJ_GOTRACER_ENABLED",
		Value:  defaults.Tracing.Enabled,
	})

	opts.tracingDSN = app.String(cli.StringOpt{
		Name:   "gotracer-collector-dsn",
		Desc:   "Otel collector DSN.",
		EnvVar: "WEB3INJ_GOTRACER_COLLECTOR_DSN",
		Value:  defaults.Tracing.CollectorDSN,
	})
}
