package filters

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
)

// unreadableBlockBackend serves logs for every block except height 2.
type unreadableBlockBackend struct{ Backend }

func (unreadableBlockBackend) HeaderByNumber(types.BlockNumber) (*ethtypes.Header, error) {
	return &ethtypes.Header{Number: big.NewInt(3)}, nil
}

func (unreadableBlockBackend) GetBlockBloomByHeight(int64) (ethtypes.Bloom, error) {
	var bloom ethtypes.Bloom
	for i := range bloom {
		bloom[i] = 0xff
	}
	return bloom, nil
}

func (unreadableBlockBackend) GetFilteredLogsByHeight(height int64, _ []common.Address, _ [][]common.Hash) ([]*virtual.RPCLog, error) {
	if height == 2 {
		return nil, errors.New("block 2 is unreadable")
	}
	return []*virtual.RPCLog{{BlockNumber: 0}}, nil
}

// A range query must fail rather than return a prefix of the logs: clients
// would advance their cursor past logs they never received.
func TestRangeLogsFailOnUnreadableBlock(t *testing.T) {
	filter := NewRangeFilter(slog.Default(), unreadableBlockBackend{}, 1, 3, nil, nil)

	logs, err := filter.Logs(context.Background(), 10_000, 10_000)

	require.ErrorContains(t, err, "height 2")
	require.Empty(t, logs)
}
