package filters

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/client"

	"log/slog"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	backendpkg "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
	streamtypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/stream"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

// FilterAPI gathers
type FilterAPI interface {
	NewPendingTransactionFilter() rpc.ID
	NewBlockFilter() rpc.ID
	NewFilter(criteria filters.FilterCriteria) (rpc.ID, error)
	GetFilterChanges(id rpc.ID) (interface{}, error)
	GetFilterLogs(ctx context.Context, id rpc.ID) ([]*virtualbank.RPCLog, error)
	UninstallFilter(id rpc.ID) bool
	GetLogs(ctx context.Context, crit filters.FilterCriteria) ([]*virtualbank.RPCLog, error)
}

// Backend defines the methods requided by the PublicFilterAPI backend
type Backend interface {
	GetBlockByNumber(blockNum types.BlockNumber, fullTx bool) (map[string]interface{}, error)
	HeaderByNumber(blockNum types.BlockNumber) (*ethtypes.Header, error)
	HeaderByHash(blockHash common.Hash) (*ethtypes.Header, error)
	TendermintBlockByHash(hash common.Hash) (*coretypes.ResultBlock, error)
	TendermintBlockResultByNumber(height *int64) (*coretypes.ResultBlockResults, error)
	GetLogs(blockHash common.Hash) ([][]*virtualbank.RPCLog, error)
	GetLogsByHeight(*int64) ([][]*virtualbank.RPCLog, error)
	GetFilteredLogs(blockHash common.Hash, addresses []common.Address, topics [][]common.Hash) ([]*virtualbank.RPCLog, error)
	GetFilteredLogsByHeight(height int64, addresses []common.Address, topics [][]common.Hash) ([]*virtualbank.RPCLog, error)
	GetBlockBloomByHeight(height int64) (ethtypes.Bloom, error)
	BlockBloom(blockRes *coretypes.ResultBlockResults) (ethtypes.Bloom, error)

	BloomStatus() (uint64, uint64)

	RPCFilterCap() int32
	RPCLogsCap() int32
	RPCBlockRangeCap() int32
}

// consider a filter inactive if it has not been polled for within deadline
var deadline = 5 * time.Minute

// filter is a helper struct that holds meta information over the filter type
// and associated subscription in the event system.
type filter struct {
	typ      filters.Type
	deadline *time.Timer // filter is inactive when deadline triggers
	crit     filters.FilterCriteria
	offset   int // offset for stream subscription
}

// PublicFilterAPI offers support to create and manage filters. This will allow external clients to retrieve various
// information related to the Ethereum protocol such as blocks, transactions and logs.
type PublicFilterAPI struct {
	logger    *slog.Logger
	clientCtx client.Context
	backend   Backend
	events    *streamtypes.RPCStream
	filtersMu sync.Mutex
	filters   map[rpc.ID]*filter
}

// NewPublicAPI returns a new PublicFilterAPI instance.
func NewPublicAPI(logger *slog.Logger, clientCtx client.Context, stream *streamtypes.RPCStream, backend Backend) *PublicFilterAPI {
	logger = logger.With("api", "filter")

	api := &PublicFilterAPI{
		logger:    logger,
		clientCtx: clientCtx,
		backend:   backend,
		filters:   make(map[rpc.ID]*filter),
		events:    stream,
	}

	go api.timeoutLoop()

	return api
}

func (api *PublicFilterAPI) liveFiltersAvailable() bool {
	return api.events != nil
}

// timeoutLoop runs every 5 minutes and deletes filters that have not been recently used.
// Tt is started when the api is created.
func (api *PublicFilterAPI) timeoutLoop() {
	ticker := time.NewTicker(deadline)
	defer ticker.Stop()

	for {
		<-ticker.C
		api.filtersMu.Lock()
		for id, f := range api.filters {
			select {
			case <-f.deadline.C:
				delete(api.filters, id)
			default:
				continue
			}
		}
		api.filtersMu.Unlock()
	}
}

// NewPendingTransactionFilter creates a filter that fetches pending transaction hashes
// as transactions enter the pending state.
//
// It is part of the filter package because this filter can be used through the
// `eth_getFilterChanges` polling method that is also used for log filters.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_newPendingTransactionFilter
func (api *PublicFilterAPI) NewPendingTransactionFilter() rpc.ID {
	if !api.liveFiltersAvailable() {
		return rpc.ID("error creating pending tx filter: live filters unavailable in polling-only mode")
	}

	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if len(api.filters) >= int(api.backend.RPCFilterCap()) {
		return rpc.ID("error creating pending tx filter: max limit reached")
	}

	id := rpc.NewID()
	_, offset := api.events.PendingTxStream().ReadNonBlocking(-1)
	api.filters[id] = &filter{
		typ:      filters.PendingTransactionsSubscription,
		deadline: time.NewTimer(deadline),
		offset:   offset,
	}

	return id
}

// NewBlockFilter creates a filter that fetches blocks that are imported into the chain.
// It is part of the filter package since polling goes with eth_getFilterChanges.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_newblockfilter
func (api *PublicFilterAPI) NewBlockFilter() rpc.ID {
	if !api.liveFiltersAvailable() {
		return rpc.ID("error creating block filter: live filters unavailable in polling-only mode")
	}

	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if len(api.filters) >= int(api.backend.RPCFilterCap()) {
		return rpc.ID("error creating block filter: max limit reached")
	}

	id := rpc.NewID()
	_, offset := api.events.HeaderStream().ReadNonBlocking(-1)
	api.filters[id] = &filter{
		typ:      filters.BlocksSubscription,
		deadline: time.NewTimer(deadline),
		offset:   offset,
	}

	return id
}

// NewFilter creates a new filter and returns the filter id. It can be
// used to retrieve logs when the state changes. This method cannot be
// used to fetch logs that are already stored in the state.
//
// Default criteria for the from and to block are "latest".
// Using "latest" as block number will return logs for mined blocks.
// Using "pending" as block number returns logs for not yet mined (pending) blocks.
// In case logs are removed (chain reorg) previously returned logs are returned
// again but with the removed property set to true.
//
// In case "fromBlock" > "toBlock" an error is returned.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_newfilter
func (api *PublicFilterAPI) NewFilter(criteria filters.FilterCriteria) (rpc.ID, error) {
	if !api.liveFiltersAvailable() {
		return rpc.ID(""), fmt.Errorf("error creating filter: live filters unavailable in polling-only mode")
	}

	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if err := ValidateFilterCriteria(criteria); err != nil {
		return rpc.ID(""), errors.Wrap(err, "error creating filter: invalid criteria")
	}

	if len(api.filters) >= int(api.backend.RPCFilterCap()) {
		return rpc.ID(""), fmt.Errorf("error creating filter: max limit reached")
	}

	id := rpc.NewID()
	_, offset := api.events.LogStream().ReadNonBlocking(-1)
	api.filters[id] = &filter{
		typ:      filters.LogsSubscription,
		deadline: time.NewTimer(deadline),
		crit:     criteria,
		offset:   offset,
	}

	return id, nil
}

// GetLogs returns logs matching the given argument. The backend decides whether
// the block data comes from indexed/cache storage or live block results, and may
// include virtualized Cosmos x/bank logs when that mode is enabled.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_getlogs
func (api *PublicFilterAPI) GetLogs(ctx context.Context, crit filters.FilterCriteria) ([]*virtualbank.RPCLog, error) {
	defer gotracer.Trace(&ctx)()

	if err := ValidateFilterCriteria(crit); err != nil {
		return nil, errors.Wrap(err, "invalid criteria")
	}

	backend := api.backend
	if carrier, ok := api.backend.(interface {
		WithContext(context.Context) backendpkg.EVMBackend
	}); ok {
		backend = carrier.WithContext(ctx)
	}

	var filter *Filter
	if crit.BlockHash != nil {
		// Block filter requested, construct a single-shot filter
		filter = NewBlockFilter(api.logger, backend, crit)
	} else {
		// Convert the RPC block numbers into internal representations
		begin := rpc.LatestBlockNumber.Int64()
		if crit.FromBlock != nil {
			begin = crit.FromBlock.Int64()
		}
		end := rpc.LatestBlockNumber.Int64()
		if crit.ToBlock != nil {
			end = crit.ToBlock.Int64()
		}
		// Construct the range filter
		filter = NewRangeFilter(api.logger, backend, begin, end, crit.Addresses, crit.Topics)
	}

	// Run the filter and return all the logs
	logs, err := filter.Logs(ctx, int(api.backend.RPCLogsCap()), int64(api.backend.RPCBlockRangeCap()))
	if err != nil {
		return nil, err
	}

	return returnLogs(logs), err
}

// UninstallFilter removes the filter with the given filter id.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_uninstallfilter
func (api *PublicFilterAPI) UninstallFilter(id rpc.ID) bool {
	api.filtersMu.Lock()
	_, found := api.filters[id]
	if found {
		delete(api.filters, id)
	}
	api.filtersMu.Unlock()

	return found
}

// GetFilterLogs returns the logs for the filter with the given id by running
// the saved criteria through the same cache-first/live fallback path as
// eth_getLogs.
// If the filter could not be found an empty array of logs is returned.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_getfilterlogs
func (api *PublicFilterAPI) GetFilterLogs(ctx context.Context, id rpc.ID) ([]*virtualbank.RPCLog, error) {
	defer gotracer.Trace(&ctx)()

	api.filtersMu.Lock()
	f, found := api.filters[id]
	api.filtersMu.Unlock()

	if !found {
		return returnLogs(nil), fmt.Errorf("filter %s not found", id)
	}

	if f.typ != filters.LogsSubscription {
		return returnLogs(nil), fmt.Errorf("filter %s doesn't have a LogsSubscription type: got %d", id, f.typ)
	}

	backend := api.backend
	if carrier, ok := api.backend.(interface {
		WithContext(context.Context) backendpkg.EVMBackend
	}); ok {
		backend = carrier.WithContext(ctx)
	}

	var filter *Filter
	if f.crit.BlockHash != nil {
		// Block filter requested, construct a single-shot filter
		filter = NewBlockFilter(api.logger, backend, f.crit)
	} else {
		// Convert the RPC block numbers into internal representations
		begin := rpc.LatestBlockNumber.Int64()
		if f.crit.FromBlock != nil {
			begin = f.crit.FromBlock.Int64()
		}
		end := rpc.LatestBlockNumber.Int64()
		if f.crit.ToBlock != nil {
			end = f.crit.ToBlock.Int64()
		}
		// Construct the range filter
		filter = NewRangeFilter(api.logger, backend, begin, end, f.crit.Addresses, f.crit.Topics)
	}
	// Run the filter and return all the logs
	logs, err := filter.Logs(ctx, int(api.backend.RPCLogsCap()), int64(api.backend.RPCBlockRangeCap()))
	if err != nil {
		return nil, err
	}
	return returnLogs(logs), nil
}

// GetFilterChanges returns the logs for the filter with the given id since
// last time it was called. This can be used for polling.
//
// For pending transaction and block filters the result is []common.Hash.
// (pending)Log filters return []Log.
//
// https://github.com/ethereum/wiki/wiki/JSON-RPC#eth_getfilterchanges
func (api *PublicFilterAPI) GetFilterChanges(id rpc.ID) (interface{}, error) {
	if !api.liveFiltersAvailable() {
		return nil, fmt.Errorf("filter changes unavailable in polling-only mode")
	}

	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	f, found := api.filters[id]
	if !found {
		return nil, fmt.Errorf("filter %s not found", id)
	}

	if !f.deadline.Stop() {
		// timer expired but filter is not yet removed in timeout loop
		// receive timer value and reset timer
		<-f.deadline.C
	}
	f.deadline.Reset(deadline)

	switch f.typ {
	case filters.PendingTransactionsSubscription:
		var hashes []common.Hash
		hashes, f.offset = api.events.PendingTxStream().ReadAllNonBlocking(f.offset)
		return returnHashes(hashes), nil
	case filters.BlocksSubscription:
		var headers []streamtypes.RPCHeader
		headers, f.offset = api.events.HeaderStream().ReadAllNonBlocking(f.offset)
		hashes := make([]common.Hash, len(headers))
		for i, header := range headers {
			hashes[i] = header.Hash
		}
		return hashes, nil
	case filters.LogsSubscription:
		var (
			logs  []*virtualbank.RPCLog
			chunk []*ethtypes.Log
		)
		for {
			chunk, f.offset = api.events.LogStream().ReadNonBlocking(f.offset)
			if len(chunk) == 0 {
				break
			}
			filtered := FilterLogs(virtualbank.WrapLogs(chunk, false, nil), f.crit.FromBlock, f.crit.ToBlock, f.crit.Addresses, f.crit.Topics)
			logs = append(logs, filtered...)
		}
		return returnLogs(logs), nil
	default:
		return nil, fmt.Errorf("invalid filter %s type %d", id, f.typ)
	}
}
