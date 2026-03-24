package telemetry

import (
	"fmt"
	"log/slog"

	"github.com/alexcesaro/statsd"

	"github.com/InjectiveLabs/web3-gateway/internal/config"
)

type StatsdClient interface {
	Close()
}

func InitStatsd(cfg config.StatsdConfig, env string, logger *slog.Logger) (StatsdClient, error) {
	if !cfg.Enabled {
		logger.Info("statsd disabled")
		return nil, nil
	}

	prefix := cfg.Prefix
	if prefix != "" && prefix[len(prefix)-1] != '.' {
		prefix += "."
	}

	client, err := statsd.New(
		statsd.Address(cfg.Addr),
		statsd.Prefix(prefix),
		statsd.ErrorHandler(func(err error) {
			logger.Warn("statsd error", "error", err)
		}),
		statsd.TagsFormat(statsd.InfluxDB),
		statsd.Tags("env", env),
	)
	if err != nil {
		return nil, fmt.Errorf("statsd init: %w", err)
	}

	logger.Info("statsd enabled", "addr", cfg.Addr, "prefix", prefix)
	return client, nil
}
