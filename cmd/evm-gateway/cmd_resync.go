package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	gatewayapp "github.com/InjectiveLabs/evm-gateway/internal/app"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/logging"
	"github.com/pkg/errors"
)

var resyncRunner = runResync

func runResync(opts *gatewayCLIOptions, args []string) error {
	targets, err := parseResyncTargets(args)
	if err != nil {
		return err
	}

	cfg, err := buildConfig(opts)
	if err != nil {
		return err
	}

	logger := logging.New(logging.Config{
		Format:  cfg.LogFormat,
		Verbose: cfg.LogVerbose,
		Output:  os.Stdout,
	})

	initSDKConfig()

	totalBlocks := indexer.CountBlocks(targets)
	logger.Info("resync targets", "segments", len(targets), "unique_blocks", totalBlocks)
	for _, target := range targets {
		logger.Info("resync target segment", "start", target.Start, "end", target.End, "blocks", target.End-target.Start+1)
	}

	if err := gatewayapp.RunResync(cfg, logger, targets); err != nil {
		logger.Error("resync failed", "error", err)
		return err
	}

	return nil
}

func parseResyncTargets(args []string) ([]indexer.BlockRange, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no resync targets provided")
	}

	targets := make([]indexer.BlockRange, 0, len(args))
	for _, arg := range args {
		target, err := parseResyncTarget(arg)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}

	return indexer.NormalizeRanges(targets), nil
}

func parseResyncTarget(raw string) (indexer.BlockRange, error) {
	if raw == "" {
		return indexer.BlockRange{}, fmt.Errorf("empty resync target")
	}

	if !strings.Contains(raw, ":") {
		height, err := parseResyncHeight(raw)
		if err != nil {
			return indexer.BlockRange{}, errors.Wrapf(err, "parse resync target %q", raw)
		}
		return indexer.BlockRange{Start: height, End: height}, nil
	}

	if strings.Count(raw, ":") != 1 {
		return indexer.BlockRange{}, fmt.Errorf("parse resync target %q: expected START:END", raw)
	}

	startRaw, endRaw, _ := strings.Cut(raw, ":")
	start, err := parseResyncHeight(startRaw)
	if err != nil {
		return indexer.BlockRange{}, errors.Wrapf(err, "parse resync target %q", raw)
	}
	end, err := parseResyncHeight(endRaw)
	if err != nil {
		return indexer.BlockRange{}, errors.Wrapf(err, "parse resync target %q", raw)
	}
	if start > end {
		return indexer.BlockRange{}, fmt.Errorf("parse resync target %q: start block %d greater than end block %d", raw, start, end)
	}

	return indexer.BlockRange{Start: start, End: end}, nil
}

func parseResyncHeight(raw string) (int64, error) {
	height, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid block height %q", raw)
	}
	if height < 0 {
		return 0, fmt.Errorf("block height %d cannot be negative", height)
	}
	return height, nil
}
