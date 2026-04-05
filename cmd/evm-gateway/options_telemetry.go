package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initTelemetryOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.statsdEnabled = app.Bool(cli.BoolOpt{
		Name:   "statsd-enabled",
		Desc:   "Enable statsd.",
		EnvVar: "WEB3INJ_STATSD_ENABLED",
		Value:  defaults.Statsd.Enabled,
	})

	opts.statsdAddr = app.String(cli.StringOpt{
		Name:   "statsd-addr",
		Desc:   "Statsd address.",
		EnvVar: "WEB3INJ_STATSD_ADDR",
		Value:  defaults.Statsd.Addr,
	})

	opts.statsdPrefix = app.String(cli.StringOpt{
		Name:   "statsd-prefix",
		Desc:   "Statsd prefix.",
		EnvVar: "WEB3INJ_STATSD_PREFIX",
		Value:  defaults.Statsd.Prefix,
	})

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
