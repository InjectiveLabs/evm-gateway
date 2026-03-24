package telemetry

import (
	"log/slog"
	"runtime/debug"

	"upd.dev/xlab/gotracer"
	gotracerotel "upd.dev/xlab/gotracer/exporters/otel"

	"github.com/InjectiveLabs/web3-gateway/internal/config"
)

func InitTracing(cfg config.TracingConfig, env string, logger *slog.Logger) {
	if !cfg.Enabled {
		logger.Info("gotracer disabled")
		return
	}

	serviceVersion := "dev"
	if info, ok := debug.ReadBuildInfo(); ok {
		serviceVersion = info.Main.Version
	}

	headers := map[string]string{}
	if cfg.CollectorAuthorization != "" {
		headerName := cfg.CollectorAuthorizationField
		if headerName == "" {
			headerName = "authorization"
		}
		headers[headerName] = cfg.CollectorAuthorization
	}

	tracerCfg := gotracer.DefaultConfig()
	tracerCfg.Enabled = true
	tracerCfg.EnvName = env
	tracerCfg.ServiceName = "web3-gateway"
	tracerCfg.ServiceVersion = serviceVersion
	tracerCfg.ClusterID = cfg.ClusterID
	tracerCfg.CollectorDSN = cfg.CollectorDSN
	tracerCfg.CollectorSecureSSL = cfg.CollectorEnableTLS
	tracerCfg.CollectorHeaders = headers
	tracerCfg.Logger = logger

	gotracer.Enable(tracerCfg, gotracerotel.InitExporter)
	logger.Info("gotracer enabled", "collector", cfg.CollectorDSN)
}
