package filters

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"testing"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

type earliestLogBackend struct {
	Backend      // Unexpected paths fail the test instead of fabricating data.
	floor, head  int64
	resolveCalls int
	resolveErr   error
	failAt       int64
	failBloom    bool
	seen         []int64
}

func (b *earliestLogBackend) ResolveEarliestBlockNumber(number rpctypes.BlockNumber, capability string) (rpctypes.BlockNumber, error) {
	if number != rpctypes.EthEarliestBlockNumber || capability != "range" {
		panic("wrong earliest range contract")
	}
	b.resolveCalls++
	return rpctypes.BlockNumber(b.floor), b.resolveErr
}

func (b *earliestLogBackend) HeaderByNumber(number rpctypes.BlockNumber) (*ethtypes.Header, error) {
	if number != rpctypes.EthLatestBlockNumber {
		panic("head query must remain latest")
	}
	return &ethtypes.Header{Number: big.NewInt(b.head)}, nil
}

func (b *earliestLogBackend) GetBlockBloomByHeight(height int64) (ethtypes.Bloom, error) {
	if b.failBloom && height == b.failAt {
		return ethtypes.Bloom{}, errors.New("shard missing bloom")
	}
	return ethtypes.Bloom{}, nil
}

func (b *earliestLogBackend) GetFilteredLogsByHeight(height int64, _ []common.Address, _ [][]common.Hash) ([]*virtualbank.RPCLog, error) {
	b.seen = append(b.seen, height)
	if !b.failBloom && height == b.failAt {
		return nil, errors.New("shard missing logs")
	}
	return []*virtualbank.RPCLog{{}}, nil
}

func TestStitchEarliestLogBoundariesResolveOnceAndPreserveFilter(t *testing.T) {
	const floor int64 = 127250000
	for _, tc := range []struct {
		name     string
		from, to int64
		want     []int64
	}{
		{"both earliest", 0, 0, []int64{floor}},
		{"from earliest", 0, floor + 2, []int64{floor, floor + 1, floor + 2}},
		{"to earliest", floor, 0, []int64{floor}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &earliestLogBackend{floor: floor, head: floor + 5}
			f := NewRangeFilter(slog.New(slog.NewTextHandler(io.Discard, nil)), b, tc.from, tc.to, nil, nil)
			for run := 1; run <= 2; run++ {
				b.seen = nil
				logs, err := f.Logs(context.Background(), 10, 10)
				if err != nil || len(logs) != len(tc.want) || !reflect.DeepEqual(b.seen, tc.want) {
					t.Fatalf("range=%v logs=%d err=%v; want %v", b.seen, len(logs), err, tc.want)
				}
				if b.resolveCalls != run {
					t.Fatalf("wanted one discovery per operation, got %d", b.resolveCalls)
				}
				if f.criteria.FromBlock.Int64() != tc.from || f.criteria.ToBlock.Int64() != tc.to {
					t.Fatal("shared filter criteria mutated")
				}
			}
		})
	}
}

func TestStitchDisabledLogResolverRetainsHeightOne(t *testing.T) {
	b := &earliestLogBackend{floor: 0, head: 3}
	f := NewRangeFilter(slog.New(slog.NewTextHandler(io.Discard, nil)), b, 0, 2, nil, nil)
	_, err := f.Logs(context.Background(), 10, 10)
	if err != nil || !reflect.DeepEqual(b.seen, []int64{1, 2}) {
		t.Fatalf("disabled earliest behavior changed: %v %v", b.seen, err)
	}
}

func TestStitchEarliestLogRangeNeverReturnsPartialSuccess(t *testing.T) {
	const floor int64 = 127250000
	for _, failure := range []string{"discovery", "bloom", "logs", "range limit"} {
		t.Run(failure, func(t *testing.T) {
			b := &earliestLogBackend{floor: floor, head: floor + 3, failAt: floor + 1}
			limit := int64(10)
			switch failure {
			case "discovery":
				b.resolveErr = errors.New("incomplete shard discovery")
			case "bloom":
				b.failBloom = true
			case "range limit":
				limit = 1
			}
			f := NewRangeFilter(slog.New(slog.NewTextHandler(io.Discard, nil)), b, 0, floor+2, nil, nil)
			logs, err := f.Logs(context.Background(), 10, limit)
			if err == nil || logs != nil {
				t.Fatalf("%s became partial success: %v %v", failure, logs, err)
			}
		})
	}
}
