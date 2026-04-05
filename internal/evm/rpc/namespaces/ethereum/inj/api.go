package inj

import (
	"context"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	"upd.dev/xlab/gotracer"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
)

type InjectiveAPI interface {
	GetTxHashByEthHash(context.Context, common.Hash) (common.Hash, error)
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
func (api *API) GetTxHashByEthHash(ctx context.Context, ethHash common.Hash) (common.Hash, error) {
	defer gotracer.Trace(&ctx)()
	api.logger.Debug("inj_GetTxHashByEthHash")
	return api.backend.WithContext(ctx).GetTxHashByEthHash(ethHash)
}
