package main

import (
	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initChainOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.chainID = app.String(cli.StringOpt{
		Name:   "chain-id",
		Desc:   "Expected Cosmos / CometBFT chain ID.",
		EnvVar: "WEB3INJ_CHAIN_ID",
		Value:  defaults.ChainID,
	})

	opts.evmChainID = app.String(cli.StringOpt{
		Name:   "evm-chain-id",
		Desc:   "Expected EVM chain ID. Optional online; recommended for offline RPC-only mode.",
		EnvVar: "WEB3INJ_EVM_CHAIN_ID",
		Value:  defaults.EVMChainID,
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
