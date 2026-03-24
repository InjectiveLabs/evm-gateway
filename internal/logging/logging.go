package logging

import (
	"io"
	"log/slog"
)

type Config struct {
	Format  string
	Verbose bool
	Output  io.Writer
}

func New(cfg Config) *slog.Logger {
	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	switch cfg.Format {
	case "text", "console":
		handler = slog.NewTextHandler(cfg.Output, opts)
	default:
		handler = slog.NewJSONHandler(cfg.Output, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
