package main

import (
	"os"
	"sync"

	"github.com/InjectiveLabs/sdk-go/chain/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	gatewayapp "github.com/InjectiveLabs/evm-gateway/internal/app"
	"github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/logging"
	"github.com/InjectiveLabs/evm-gateway/internal/telemetry"
)

var (
	startRunner   = runStart
	sdkConfigOnce sync.Once
)

func runStart(opts *gatewayCLIOptions) error {
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

	telemetry.InitTracing(cfg.Tracing, cfg.Env, logger)

	if err := gatewayapp.Run(cfg, logger); err != nil {
		logger.Error("evm-gateway failed", "error", err)
		return err
	}

	return nil
}

func buildConfig(opts *gatewayCLIOptions) (config.Config, error) {
	cfg := config.DefaultConfig()

	cfg.ChainID = *opts.chainID
	cfg.CometRPC = *opts.cometRPC
	cfg.GRPCAddr = *opts.grpcAddr
	cfg.Earliest = int64(*opts.earliest)
	cfg.FetchJobs = *opts.fetchJobs
	cfg.DataDir = *opts.dataDir
	cfg.LogFormat = *opts.logFormat
	cfg.LogVerbose = *opts.logVerbose
	cfg.JSONRPC.Address = *opts.rpcAddr
	cfg.JSONRPC.WsAddress = *opts.wsAddr
	cfg.JSONRPC.API = parseCSV(*opts.rpcAPI, cfg.JSONRPC.API)
	cfg.Tracing.Enabled = *opts.tracingEnabled
	cfg.Tracing.CollectorDSN = *opts.tracingDSN
	cfg.Tracing.CollectorAuthorization = *opts.tracingCollectorAuthorization
	cfg.Tracing.CollectorAuthorizationField = *opts.tracingCollectorAuthorizationName
	cfg.Tracing.CollectorEnableTLS = *opts.tracingCollectorEnableTLS

	cfg.Expand()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func initSDKConfig() {
	sdkConfigOnce.Do(func() {
		sdkConfig := sdk.GetConfig()
		types.SetBech32Prefixes(sdkConfig)
		sdkConfig.Seal()
	})
}
