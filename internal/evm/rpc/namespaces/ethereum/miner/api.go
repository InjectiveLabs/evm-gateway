package miner

import (
	"log/slog"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
)

// API is the private miner prefixed set of APIs in the Miner JSON-RPC spec.
type API struct {
	logger  *slog.Logger
	backend backend.EVMBackend
}

// NewPrivateAPI creates an instance of the Miner API.
func NewPrivateAPI(
	logger *slog.Logger,
	evmBackend backend.EVMBackend,
) *API {
	return &API{
		logger:  logger.With("api", "miner"),
		backend: evmBackend,
	}
}

// SetGasPrice sets the minimum accepted gas price for the miner.
func (api *API) SetGasPrice(gasPrice hexutil.Big) bool {
	api.logger.Info("miner_setGasPrice")
	return api.backend.SetGasPrice(gasPrice)
}
