package main

import (
	"strings"

	cli "github.com/jawher/mow.cli"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
)

func initRPCOptions(app *cli.Cli, opts *gatewayCLIOptions, defaults config.Config) {
	opts.enableRPC = app.Bool(cli.BoolOpt{
		Name:   "rpc-enable",
		Desc:   "Enable JSON-RPC server.",
		EnvVar: "WEB3INJ_JSONRPC_ENABLE",
		Value:  defaults.JSONRPC.Enable,
	})

	opts.rpcAddr = app.String(cli.StringOpt{
		Name:   "rpc-address",
		Desc:   "JSON-RPC HTTP listen address.",
		EnvVar: "WEB3INJ_JSONRPC_ADDRESS",
		Value:  defaults.JSONRPC.Address,
	})

	opts.wsAddr = app.String(cli.StringOpt{
		Name:   "ws-address",
		Desc:   "JSON-RPC WS listen address.",
		EnvVar: "WEB3INJ_JSONRPC_WS_ADDRESS",
		Value:  defaults.JSONRPC.WsAddress,
	})

	opts.rpcAPI = app.String(cli.StringOpt{
		Name:   "rpc-api",
		Desc:   "Comma-separated JSON-RPC API namespaces.",
		EnvVar: "WEB3INJ_JSONRPC_API",
		Value:  strings.Join(defaults.JSONRPC.API, ","),
	})
}
