package filters

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"

	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethfilters "github.com/ethereum/go-ethereum/eth/filters"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	backendpkg "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
)

// BloomIV represents the bit indexes and value inside the bloom filter that belong
// to some key.
type BloomIV struct {
	I [3]uint
	V [3]byte
}

// Filter can be used to retrieve and filter logs.
type Filter struct {
	logger   *slog.Logger
	backend  Backend
	criteria ethfilters.FilterCriteria

	bloomFilters [][]BloomIV // Filter the system is matching for
}

// NewBlockFilter creates a new filter which directly inspects the contents of
// a block to figure out whether it is interesting or not.
func NewBlockFilter(logger *slog.Logger, backend Backend, criteria ethfilters.FilterCriteria) *Filter {
	// Create a generic filter and convert it into a block filter
	return newFilter(logger, backend, criteria, nil)
}

// NewRangeFilter creates a new filter which uses a bloom filter on blocks to
// figure out whether a particular block is interesting or not.
func NewRangeFilter(logger *slog.Logger, backend Backend, begin, end int64, addresses []common.Address, topics [][]common.Hash) *Filter {
	// Flatten the address and topic filter clauses into a single bloombits filter
	// system. Since the bloombits are not positional, nil topics are permitted,
	// which get flattened into a nil byte slice.
	filtersBz := make([][][]byte, 0, len(addresses)+len(topics))
	if len(addresses) > 0 {
		filter := make([][]byte, len(addresses))
		for i, address := range addresses {
			filter[i] = address.Bytes()
		}
		filtersBz = append(filtersBz, filter)
	}

	for _, topicList := range topics {
		filter := make([][]byte, len(topicList))
		for i, topic := range topicList {
			filter[i] = topic.Bytes()
		}
		filtersBz = append(filtersBz, filter)
	}

	// Create a generic filter and convert it into a range filter
	criteria := ethfilters.FilterCriteria{
		FromBlock: big.NewInt(begin),
		ToBlock:   big.NewInt(end),
		Addresses: addresses,
		Topics:    topics,
	}

	return newFilter(logger, backend, criteria, createBloomFilters(filtersBz, logger))
}

// newFilter returns a new Filter
func newFilter(logger *slog.Logger, backend Backend, criteria ethfilters.FilterCriteria, bloomFilters [][]BloomIV) *Filter {
	return &Filter{
		logger:       logger,
		backend:      backend,
		criteria:     criteria,
		bloomFilters: bloomFilters,
	}
}

const (
	maxToOverhang = 600
)

// Logs searches the requested block hash or range for matching RPC-visible log
// entries. Backend lookups may be served from indexed/cache data or from live
// Comet results, depending on cache coverage and virtualization mode.
func (f *Filter) Logs(ctx context.Context, logLimit int, blockLimit int64) ([]*virtualbank.RPCLog, error) {
	defer gotracer.Trace(&ctx)()

	backend := f.backend
	if carrier, ok := f.backend.(interface {
		WithContext(context.Context) backendpkg.EVMBackend
	}); ok {
		backend = carrier.WithContext(ctx)
	}

	// If we're doing singleton block filtering, execute and return
	if f.criteria.BlockHash != nil && *f.criteria.BlockHash != (common.Hash{}) {
		header, err := backend.HeaderByHash(*f.criteria.BlockHash)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to fetch header by hash %s", f.criteria.BlockHash)
		}
		if header == nil || header.Number == nil {
			return []*virtualbank.RPCLog{}, nil
		}
		height := header.Number.Int64()

		bloom, err := backend.GetBlockBloomByHeight(height)
		if err != nil {
			f.logger.Debug("failed to fetch block bloom", "height", height, "error", err.Error())
			return nil, nil
		}
		if !bloomFilter(bloom, f.criteria.Addresses, f.criteria.Topics) {
			return []*virtualbank.RPCLog{}, nil
		}

		return f.blockLogsByHash(*f.criteria.BlockHash)
	}

	// Figure out the limits of the filter range
	header, err := backend.HeaderByNumber(types.EthLatestBlockNumber)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch header by number (latest)")
	}

	if header == nil || header.Number == nil {
		f.logger.Debug("header not found or has no number")
		return nil, nil
	}

	head := header.Number.Int64()
	if f.criteria.FromBlock.Int64() < 0 {
		f.criteria.FromBlock = big.NewInt(head)
	} else if f.criteria.FromBlock.Int64() == 0 {
		f.criteria.FromBlock = big.NewInt(1)
	}
	if f.criteria.ToBlock.Int64() < 0 {
		f.criteria.ToBlock = big.NewInt(head)
	} else if f.criteria.ToBlock.Int64() == 0 {
		f.criteria.ToBlock = big.NewInt(1)
	}

	if f.criteria.ToBlock.Int64()-f.criteria.FromBlock.Int64() > blockLimit {
		return nil, fmt.Errorf("maximum [from, to] blocks distance: %d", blockLimit)
	}

	// check bounds
	if f.criteria.FromBlock.Int64() > head {
		return []*virtualbank.RPCLog{}, nil
	} else if f.criteria.ToBlock.Int64() > head+maxToOverhang {
		f.criteria.ToBlock = big.NewInt(head + maxToOverhang)
	}

	from := f.criteria.FromBlock.Int64()
	to := f.criteria.ToBlock.Int64()
	logs := []*virtualbank.RPCLog{}

	for height := from; height <= to; height++ {
		bloom, err := backend.GetBlockBloomByHeight(height)
		if err != nil {
			f.logger.Debug("failed to fetch block bloom", "height", height, "error", err.Error())
			return logs, nil
		}
		if !bloomFilter(bloom, f.criteria.Addresses, f.criteria.Topics) {
			continue
		}

		filtered, err := f.blockLogsByHeight(height)
		if err != nil {
			f.logger.Debug("failed to fetch block logs", "height", height, "error", err.Error())
			return logs, nil
		}

		// check logs limit
		if len(logs)+len(filtered) > logLimit {
			return logs, fmt.Errorf("query returned more than %d results", logLimit)
		}
		logs = append(logs, filtered...)
	}
	return logs, nil
}

// blockLogsByHash delegates a single-block hash query to the backend's
// cache-first filtered log path.
func (f *Filter) blockLogsByHash(hash common.Hash) ([]*virtualbank.RPCLog, error) {
	return f.backend.GetFilteredLogs(hash, f.criteria.Addresses, f.criteria.Topics)
}

// blockLogsByHeight delegates a single-block height query to the backend's
// cache-first filtered log path.
func (f *Filter) blockLogsByHeight(height int64) ([]*virtualbank.RPCLog, error) {
	return f.backend.GetFilteredLogsByHeight(height, f.criteria.Addresses, f.criteria.Topics)
}

func createBloomFilters(filters [][][]byte, logger *slog.Logger) [][]BloomIV {
	bloomFilters := make([][]BloomIV, 0)
	for _, filter := range filters {
		// Gather the bit indexes of the filter rule, special casing the nil filter
		if len(filter) == 0 {
			continue
		}
		bloomIVs := make([]BloomIV, len(filter))

		// Transform the filter rules (the addresses and topics) to the bloom index and value arrays
		// So it can be used to compare with the bloom of the block header. If the rule has any nil
		// clauses. The rule will be ignored.
		for i, clause := range filter {
			if clause == nil {
				bloomIVs = nil
				break
			}

			iv, err := calcBloomIVs(clause)
			if err != nil {
				bloomIVs = nil
				logger.Error("calcBloomIVs error", "error", err)
				break
			}

			bloomIVs[i] = iv
		}
		// Accumulate the filter rules if no nil rule was within
		if bloomIVs != nil {
			bloomFilters = append(bloomFilters, bloomIVs)
		}
	}
	return bloomFilters
}

// calcBloomIVs returns BloomIV for the given data,
// revised from https://github.com/ethereum/go-ethereum/blob/401354976bb44f0ad4455ca1e0b5c0dc31d9a5f5/core/types/bloom9.go#L139
func calcBloomIVs(data []byte) (BloomIV, error) {
	hashbuf := make([]byte, 6)
	biv := BloomIV{}

	sha := crypto.NewKeccakState()
	sha.Reset()
	if _, err := sha.Write(data); err != nil {
		return BloomIV{}, err
	}
	if _, err := sha.Read(hashbuf); err != nil {
		return BloomIV{}, err
	}

	// The actual bits to flip
	biv.V[0] = byte(1 << (hashbuf[1] & 0x7))
	biv.V[1] = byte(1 << (hashbuf[3] & 0x7))
	biv.V[2] = byte(1 << (hashbuf[5] & 0x7))
	// The indices for the bytes to OR in
	biv.I[0] = ethtypes.BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf)&0x7ff)>>3) - 1
	biv.I[1] = ethtypes.BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf[2:])&0x7ff)>>3) - 1
	biv.I[2] = ethtypes.BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf[4:])&0x7ff)>>3) - 1

	return biv, nil
}
