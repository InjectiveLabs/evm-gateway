package rpc

import (
	"fmt"
	"log/slog"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/debug"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/eth"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/eth/filters"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/inj"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/miner"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/net"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/txpool"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/namespaces/ethereum/web3"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/stream"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
)

// RPC namespaces and API version
const (
	// Cosmos namespaces

	CosmosNamespace = "cosmos"

	// Ethereum namespaces

	Web3Namespace      = "web3"
	EthNamespace       = "eth"
	NetNamespace       = "net"
	TxPoolNamespace    = "txpool"
	DebugNamespace     = "debug"
	MinerNamespace     = "miner"
	InjectiveNamespace = "inj"

	apiVersion = "1.0"
)

// APICreator creates the JSON-RPC API implementations.
type APICreator = func(
	logger *slog.Logger,
	cfg config.Config,
	clientCtx client.Context,
	stream *stream.RPCStream,
	allowUnprotectedTxs bool,
	indexer txindexer.TxIndexer,
	status *syncstatus.Tracker,
) []rpc.API

// apiCreators defines the JSON-RPC API namespaces.
var apiCreators map[string]APICreator

func init() {
	apiCreators = map[string]APICreator{
		EthNamespace: func(logger *slog.Logger,
			cfg config.Config,
			clientCtx client.Context,
			stream *stream.RPCStream,
			allowUnprotectedTxs bool,
			indexer txindexer.TxIndexer,
			status *syncstatus.Tracker,
		) []rpc.API {
			evmBackend := backend.NewBackend(logger, cfg, clientCtx, allowUnprotectedTxs, indexer, status)
			return []rpc.API{
				{
					Namespace: EthNamespace,
					Version:   apiVersion,
					Service:   eth.NewPublicAPI(logger, evmBackend),
				},
				{
					Namespace: EthNamespace,
					Version:   apiVersion,
					Service:   filters.NewPublicAPI(logger, clientCtx, stream, evmBackend),
				},
			}
		},
		Web3Namespace: func(*slog.Logger, config.Config, client.Context, *stream.RPCStream, bool, txindexer.TxIndexer, *syncstatus.Tracker) []rpc.API {
			return []rpc.API{
				{
					Namespace: Web3Namespace,
					Version:   apiVersion,
					Service:   web3.NewPublicAPI(),
				},
			}
		},
		NetNamespace: func(_ *slog.Logger, cfg config.Config, clientCtx client.Context, _ *stream.RPCStream, _ bool, _ txindexer.TxIndexer, _ *syncstatus.Tracker) []rpc.API {
			return []rpc.API{
				{
					Namespace: NetNamespace,
					Version:   apiVersion,
					Service:   net.NewPublicAPI(clientCtx, cfg.EVMChainID),
				},
			}
		},
		TxPoolNamespace: func(logger *slog.Logger, _ config.Config, _ client.Context, _ *stream.RPCStream, _ bool, _ txindexer.TxIndexer, _ *syncstatus.Tracker) []rpc.API {
			return []rpc.API{
				{
					Namespace: TxPoolNamespace,
					Version:   apiVersion,
					Service:   txpool.NewPublicAPI(logger),
				},
			}
		},
		DebugNamespace: func(logger *slog.Logger,
			cfg config.Config,
			clientCtx client.Context,
			_ *stream.RPCStream,
			allowUnprotectedTxs bool,
			indexer txindexer.TxIndexer,
			status *syncstatus.Tracker,
		) []rpc.API {
			evmBackend := backend.NewBackend(logger, cfg, clientCtx, allowUnprotectedTxs, indexer, status)
			return []rpc.API{
				{
					Namespace: DebugNamespace,
					Version:   apiVersion,
					Service:   debug.NewAPI(logger, evmBackend),
				},
			}
		},
		MinerNamespace: func(logger *slog.Logger,
			cfg config.Config,
			clientCtx client.Context,
			_ *stream.RPCStream,
			allowUnprotectedTxs bool,
			indexer txindexer.TxIndexer,
			status *syncstatus.Tracker,
		) []rpc.API {
			evmBackend := backend.NewBackend(logger, cfg, clientCtx, allowUnprotectedTxs, indexer, status)
			return []rpc.API{
				{
					Namespace: MinerNamespace,
					Version:   apiVersion,
					Service:   miner.NewPrivateAPI(logger, evmBackend),
				},
			}
		},
		InjectiveNamespace: func(logger *slog.Logger,
			cfg config.Config,
			clientCtx client.Context,
			_ *stream.RPCStream,
			allowUnprotectedTxs bool,
			indexer txindexer.TxIndexer,
			status *syncstatus.Tracker,
		) []rpc.API {
			evmBackend := backend.NewBackend(logger, cfg, clientCtx, allowUnprotectedTxs, indexer, status)
			return []rpc.API{
				{
					Namespace: InjectiveNamespace,
					Version:   apiVersion,
					Service:   inj.NewInjectiveAPI(logger, evmBackend),
				},
			}
		},
	}
}

// GetRPCAPIs returns the list of all APIs
func GetRPCAPIs(
	logger *slog.Logger,
	cfg config.Config,
	clientCtx client.Context,
	rpcStream *stream.RPCStream,
	allowUnprotectedTxs bool,
	indexer txindexer.TxIndexer,
	status *syncstatus.Tracker,
	selectedAPIs []string,
) []rpc.API {
	var apis []rpc.API

	for _, ns := range selectedAPIs {
		if creator, ok := apiCreators[ns]; ok {
			apis = append(apis, creator(logger, cfg, clientCtx, rpcStream, allowUnprotectedTxs, indexer, status)...)
		} else {
			logger.Error("invalid namespace value", "namespace", ns)
		}
	}

	return apis
}

// RegisterAPINamespace registers a new API namespace with the API creator.
// This function fails if the namespace is already registered.
func RegisterAPINamespace(ns string, creator APICreator) error {
	if _, ok := apiCreators[ns]; ok {
		return fmt.Errorf("duplicated api namespace %s", ns)
	}
	apiCreators[ns] = creator
	return nil
}
