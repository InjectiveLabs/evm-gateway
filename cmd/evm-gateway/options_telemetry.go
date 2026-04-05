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

	opts.tracingCollectorAuthorization = app.String(cli.StringOpt{
		Name:   "gotracer-collector-authorization",
		Desc:   "Authorization header value sent to the OTEL collector.",
		EnvVar: "WEB3INJ_GOTRACER_COLLECTOR_AUTHORIZATION",
		Value:  defaults.Tracing.CollectorAuthorization,
	})

	opts.tracingCollectorAuthorizationName = app.String(cli.StringOpt{
		Name:   "gotracer-collector-authorization-header",
		Desc:   "Authorization header name sent to the OTEL collector.",
		EnvVar: "WEB3INJ_GOTRACER_COLLECTOR_AUTHORIZATION_HEADER",
		Value:  defaults.Tracing.CollectorAuthorizationField,
	})

	opts.tracingCollectorEnableTLS = app.Bool(cli.BoolOpt{
		Name:   "gotracer-collector-enable-tls",
		Desc:   "Use TLS when connecting to the OTEL collector.",
		EnvVar: "WEB3INJ_GOTRACER_COLLECTOR_ENABLE_TLS",
		Value:  defaults.Tracing.CollectorEnableTLS,
	})
}
