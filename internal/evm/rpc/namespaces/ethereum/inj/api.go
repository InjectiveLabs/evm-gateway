package inj

import (
	"log/slog"

	"github.com/ethereum/go-ethereum/common"

	"github.com/InjectiveLabs/web3-gateway/internal/evm/rpc/backend"
)

type InjectiveAPI interface {
	GetTxHashByEthHash(common.Hash) (common.Hash, error)
}

var _ InjectiveAPI = (*API)(nil)

// API holds the Injective custom API endpoints
type API struct {
	logger  *slog.Logger
	backend backend.EVMBackend
}

// NewPrivateAPI creates an instance of the Miner API.
func NewInjectiveAPI(
	logger *slog.Logger,
	evmBackend backend.EVMBackend,
) *API {
	return &API{
		logger:  logger.With("api", "inj"),
		backend: evmBackend,
	}
}

// SetEtherbase sets the etherbase of the miner
func (api *API) GetTxHashByEthHash(ethHash common.Hash) (common.Hash, error) {
	api.logger.Debug("inj_GetTxHashByEthHash")
	return api.backend.GetTxHashByEthHash(ethHash)
}
