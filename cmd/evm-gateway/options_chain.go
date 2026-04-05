package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initChainOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.chainID = app.String(cli.StringOpt{
		Name:   "chain-id",
		Desc:   "Expected chain ID.",
		EnvVar: "WEB3INJ_CHAIN_ID",
		Value:  defaults.ChainID,
	})

	opts.cometRPC = app.String(cli.StringOpt{
		Name:   "comet-rpc",
		Desc:   "CometBFT RPC endpoint.",
		EnvVar: "WEB3INJ_COMET_RPC",
		Value:  defaults.CometRPC,
	})

	opts.grpcAddr = app.String(cli.StringOpt{
		Name:   "grpc-addr",
		Desc:   "gRPC endpoint.",
		EnvVar: "WEB3INJ_GRPC_ADDR",
		Value:  defaults.GRPCAddr,
	})
}
