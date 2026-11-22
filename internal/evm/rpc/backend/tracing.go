package backend

import (
	"encoding/json"
	"fmt"
	"time"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/bytedance/sonic"
	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"
)

// normalizeTraceTimeout treats the requested timeout as an upper bound. Apply it
// before cache lookups so oversized requests cannot reuse cached upstream
// timeout-validation errors and share results with the effective configuration.
func (b *Backend) normalizeTraceTimeout(config *rpctypes.TraceConfig) *rpctypes.TraceConfig {
	cap := b.cfg.JSONRPC.TraceTimeoutCap
	if cap <= 0 {
		cap = appconfig.DefaultTraceTimeoutCap
	}
	// Keep legacy cache keys for omitted timeouts at the upstream 30s default.
	// A custom cap must also apply when the client omits its timeout.
	if config == nil {
		if cap == appconfig.DefaultTraceTimeoutCap {
			return nil
		}
		return &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Timeout: cap.String()}}
	}
	if config.Timeout == "" {
		if cap == appconfig.DefaultTraceTimeoutCap {
			return config
		}
	} else {
		timeout, err := time.ParseDuration(config.Timeout)
		if err != nil || timeout <= cap {
			return config
		}
	}
	normalized := *config
	normalized.Timeout = cap.String()
	return &normalized
}

// TraceTransaction returns the structured logs created during the execution of EVM
// and returns them as a JSON object. Txs that failed in the ante handler are
// not Ethereum txs (their nonce was not consumed) and are not traceable.
// Visible txs that cannot be replayed (virtual txs, and messages of failed
// Cosmos txs that were reverted or never executed) get the synthetic trace of
// syntheticTraceResult, the same as their block trace entry.
func (b *Backend) TraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (interface{}, error) {
	return b.traceTransaction(hash, b.normalizeTraceTimeout(config))
}

func (b *Backend) traceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		cached, err := b.indexer.GetTraceTransaction(hash, config)
		if err == nil {
			return decodeCachedTraceTransaction(cached)
		}
		if !isIndexerCacheMiss(err) {
			if b.cfg.OfflineRPCOnly {
				return nil, err
			}
			b.logger.Debug("cached tx trace lookup failed; falling back to live rpc", "hash", hash.Hex(), "error", err.Error())
		}
		if result, ok, err := b.virtualTraceTransaction(hash, config); ok {
			return result, err
		}
		if derived, derr := b.traceTransactionFromCachedBlock(hash, config); derr == nil {
			return derived, nil
		}
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.New("transaction trace not available in offline rpc-only mode")
	}

	// Get transaction by hash
	transaction, err := b.GetTxByEthHash(hash)
	if err != nil {
		b.logger.Debug("tx not found", "hash", hash)
		return nil, err
	}

	// check if block number is 0
	if transaction.Height == 0 {
		return nil, errors.New("genesis is not traceable")
	}

	block, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(transaction.Height))
	if err != nil {
		b.logger.Debug("block not found", "height", transaction.Height)
		return nil, errors.Wrap(err, "block not found")
	}
	if block == nil {
		b.logger.Debug("block not found", "height", transaction.Height)
		return nil, errors.New("block not found")
	}

	// check tx index is not out of bound
	if uint32(len(block.Block.Txs)) <= transaction.TxIndex {
		b.logger.Debug("tx index out of bounds", "index", transaction.TxIndex, "hash", hash.String(), "height", block.Block.Height)
		return nil, fmt.Errorf("transaction not included in block %v", block.Block.Height)
	}

	txResults := b.blockTxResultsForTrace(block.Block.Height)
	var txResult *abci.ExecTxResult
	if int(transaction.TxIndex) < len(txResults) {
		txResult = txResults[transaction.TxIndex]
	}
	if rpctypes.TxAnteFailed(txResult) {
		return nil, fmt.Errorf("transaction %s failed in the ante handler in block %d and was not executed", hash.Hex(), block.Block.Height)
	}

	tx, err := b.clientCtx.TxConfig.TxDecoder()(block.Block.Txs[transaction.TxIndex])
	if err != nil {
		b.logger.Debug("tx not found", "hash", hash)
		return nil, err
	}
	if int(transaction.MsgIndex) >= len(tx.GetMsgs()) {
		return nil, fmt.Errorf("message index %d out of bounds in block %d", transaction.MsgIndex, block.Block.Height)
	}
	if errMsg := msgTraceError(txResult, int(transaction.MsgIndex), len(tx.GetMsgs())); errMsg != "" {
		rpcTx, receipt, err := b.liveTraceTx(hash)
		if err != nil {
			return nil, err
		}
		return syntheticTraceResult(rpcTx, receipt, config, errMsg)
	}

	// Predecessors replay only what changed the chain state: Cosmos txs that
	// failed in the ante handler and messages of failed Cosmos txs that were
	// reverted or never executed are left out.
	predecessors := replayedMsgs(b.traceMsgs(block.Block, txResults, int(transaction.TxIndex)))

	// add predecessor messages in current cosmos tx
	for i := 0; i < int(transaction.MsgIndex); i++ {
		ethMsg, ok := tx.GetMsgs()[i].(*evmtypes.MsgEthereumTx)
		if !ok {
			continue
		}
		predecessors = append(predecessors, ethMsg)
	}

	ethMessage, ok := tx.GetMsgs()[transaction.MsgIndex].(*evmtypes.MsgEthereumTx)
	if !ok {
		b.logger.Debug("invalid transaction type", "type", fmt.Sprintf("%T", tx))
		return nil, fmt.Errorf("invalid transaction type %T", tx)
	}

	traceTxRequest := evmtypes.QueryTraceTxRequest{
		Msg:             ethMessage,
		Predecessors:    predecessors,
		BlockNumber:     block.Block.Height,
		BlockTime:       block.Block.Time,
		BlockHash:       common.Bytes2Hex(block.BlockID.Hash),
		ProposerAddress: sdk.ConsAddress(block.Block.ProposerAddress),
		ChainId:         traceChainID(b.ChainID().ToInt(), ethMessage),
	}

	if config != nil {
		traceTxRequest.TraceConfig = b.convertConfig(config)
	}

	// minus one to get the context of block beginning
	contextHeight := transaction.Height - 1
	if contextHeight < 1 {
		// 0 is a special value in `ContextWithHeight`
		contextHeight = 1
	}
	traceResult, err := b.queryClient.TraceTx(b.contextWithHeight(contextHeight), &traceTxRequest)
	if err != nil {
		return nil, err
	}

	// Response format is unknown due to custom tracer config param
	// More information can be found here https://geth.ethereum.org/docs/dapp/tracing-filtered
	var decodedResult interface{}
	err = sonic.Unmarshal(traceResult.Data, &decodedResult)
	if err != nil {
		return nil, err
	}
	if b.indexer != nil {
		if err := b.indexer.SetTraceTransaction(hash, config, traceResult.Data); err != nil {
			b.logger.Debug("failed to cache tx trace", "hash", hash.Hex(), "error", err.Error())
		}
	}

	return decodedResult, nil
}

// virtualTraceTransaction returns the synthetic trace of an indexed virtual
// tx. ok is false when hash is not a virtual tx.
func (b *Backend) virtualTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (result interface{}, ok bool, err error) {
	lookup, isLookup := b.indexer.(virtualRPCTransactionLookup)
	if !isLookup {
		return nil, false, nil
	}
	if virtual, err := lookup.IsVirtualRPCTransaction(hash); err != nil || !virtual {
		return nil, false, nil
	}
	// served like eth_getTransactionByHash: hidden when virtualization is off
	tx, err := b.GetTransactionByHash(hash)
	if err != nil {
		return nil, true, err
	}
	receipt, err := b.GetTransactionReceipt(hash)
	if err != nil {
		return nil, true, err
	}
	if tx == nil || receipt == nil {
		return nil, true, fmt.Errorf("transaction %s not found", hash.Hex())
	}
	result, err = syntheticTraceResult(tx, receipt, config, "")
	return result, true, err
}

func (b *Backend) convertConfig(config *rpctypes.TraceConfig) *evmtypes.TraceConfig {
	if config == nil {
		return &evmtypes.TraceConfig{}
	}
	cfg := config.TraceConfig
	cfg.TracerJsonConfig = string(config.TracerConfig)
	cfg.StateOverrides = []byte(config.StateOverrides)
	cfg.BlockOverrides = []byte(config.BlockOverrides)
	return &cfg
}

// TraceBlock configures a new tracer according to the provided configuration, and
// executes all the transactions contained within. The return value will be one item
// per visible transaction, dependent on the requested tracer. Txs that failed
// in the ante handler are neither replayed nor listed.
func (b *Backend) TraceBlock(height rpctypes.BlockNumber,
	config *rpctypes.TraceConfig,
	block *cmrpctypes.ResultBlock,
) ([]*rpctypes.TxTraceResult, error) {
	return b.traceBlock(height, b.normalizeTraceTimeout(config), block)
}

func (b *Backend) traceBlock(height rpctypes.BlockNumber,
	config *rpctypes.TraceConfig,
	block *cmrpctypes.ResultBlock,
) ([]*rpctypes.TxTraceResult, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	cacheHeight, cacheable := b.traceCacheHeight(height, block)
	if b.indexer != nil && cacheable {
		cached, err := b.indexer.GetTraceBlockByHeight(cacheHeight, config)
		if err == nil {
			decoded, err := decodeCachedTraceBlock(cached)
			if err != nil {
				return nil, err
			}
			if err := b.populateTraceBlockTransactionHashes(decoded, cacheHeight, block); err != nil {
				return nil, err
			}
			decoded = b.dropLegacyTracePlaceholders(decoded)
			return b.alignTraceResults(decoded, b.traceBlockView(cacheHeight, block, nil), config), nil
		}
		if !isIndexerCacheMiss(err) {
			if b.cfg.OfflineRPCOnly {
				return nil, err
			}
			b.logger.Debug("cached block trace lookup failed; falling back to live rpc", "height", cacheHeight, "error", err.Error())
		}
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.New("block trace not available in offline rpc-only mode")
	}
	if block == nil {
		var err error
		block, err = b.TendermintBlockByNumber(height)
		if err != nil {
			return nil, errors.Wrap(err, "block not found")
		}
		if block == nil || block.Block == nil {
			return nil, errors.New("block not found")
		}
		cacheHeight = block.Block.Height
		cacheable = true
	}

	blockRes := b.blockResultsForTrace(block.Block.Height)
	var txResults []*abci.ExecTxResult
	if blockRes != nil {
		txResults = blockRes.TxResults
	}
	msgs := b.traceMsgs(block.Block, txResults, len(block.Block.Txs))
	view := b.traceBlockView(cacheHeight, block, blockRes)
	if len(msgs) == 0 {
		decodedResults := b.alignTraceResults([]*rpctypes.TxTraceResult{}, view, config)
		if b.indexer != nil && cacheable {
			cacheData, err := sonic.Marshal(decodedResults)
			if err != nil {
				return nil, err
			}
			if err := b.indexer.SetTraceBlockByHeight(cacheHeight, config, cacheData); err != nil {
				b.logger.Debug("failed to cache block trace without Ethereum transactions", "height", cacheHeight, "error", err.Error())
			}
		}
		return decodedResults, nil
	}

	decodedResults, err := b.traceBlockMsgs(height, block, msgs, view, config)
	if err != nil {
		return nil, err
	}
	decodedResults = b.alignTraceResults(decodedResults, view, config)
	if b.indexer != nil && cacheable {
		cacheData, err := sonic.Marshal(decodedResults)
		if err != nil {
			return nil, err
		}
		if err := b.indexer.SetTraceBlockByHeight(cacheHeight, config, cacheData); err != nil {
			b.logger.Debug("failed to cache block trace", "height", cacheHeight, "error", err.Error())
		}
	}

	return decodedResults, nil
}

// traceBlockMsgs replays the replayed messages of msgs in one block trace and
// returns one result per message, in order. Messages with an error get a
// synthetic failed frame of their tx in view, and alone messages are traced
// on their own.
func (b *Backend) traceBlockMsgs(
	height rpctypes.BlockNumber,
	block *cmrpctypes.ResultBlock,
	msgs []traceMsg,
	view *traceTxView,
	config *rpctypes.TraceConfig,
) ([]*rpctypes.TxTraceResult, error) {
	var replayed []*rpctypes.TxTraceResult
	if txs := replayedMsgs(msgs); len(txs) > 0 {
		traceBlockRequest := &evmtypes.QueryTraceBlockRequest{
			Txs:             txs,
			TraceConfig:     b.convertConfig(config),
			BlockNumber:     block.Block.Height,
			BlockTime:       block.Block.Time,
			BlockHash:       common.Bytes2Hex(block.BlockID.Hash),
			ProposerAddress: sdk.ConsAddress(block.Block.ProposerAddress),
			ChainId:         traceChainID(b.ChainID().ToInt(), txs...),
		}
		res, err := b.queryClient.TraceBlock(b.contextWithHeight(traceBlockContextHeight(height, block)), traceBlockRequest)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(res.Data, &replayed); err != nil {
			return nil, err
		}
		if len(replayed) != len(txs) {
			return nil, fmt.Errorf("trace result count %d does not match Ethereum transaction count %d", len(replayed), len(txs))
		}
	}

	results := make([]*rpctypes.TxTraceResult, len(msgs))
	for i, m := range msgs {
		hash := m.msg.Hash()
		switch {
		case m.err != "" && view != nil:
			results[i] = b.syntheticTraceEntry(view, hash, config, m.err)
		case m.err != "":
			results[i] = &rpctypes.TxTraceResult{Error: m.err}
		case m.alone:
			results[i] = &rpctypes.TxTraceResult{}
			result, err := b.traceTransaction(hash, config)
			if err != nil {
				results[i].Error = err.Error()
			} else {
				results[i].Result = result
			}
		default:
			results[i], replayed = replayed[0], replayed[1:]
			if results[i] == nil {
				results[i] = &rpctypes.TxTraceResult{}
			}
		}
		results[i].TxHash = hash
	}
	return results, nil
}

// traceBlockEthereumTransactions returns the Ethereum messages of a block to
// replay in a block trace, with their hashes. With txResults, txs that failed in
// the ante handler are skipped: they had no effect on chain and are not visible
// over JSON-RPC.
func (b *Backend) traceBlockEthereumTransactions(
	block *cmrpctypes.ResultBlock,
	txResults []*abci.ExecTxResult,
) ([]*evmtypes.MsgEthereumTx, []common.Hash) {
	messages := b.ethMsgsFromBlock(block.Block, txResults, len(block.Block.Txs))
	hashes := make([]common.Hash, 0, len(messages))
	for _, msg := range messages {
		hashes = append(hashes, msg.Hash())
	}
	return messages, hashes
}

// blockTxResultsForTrace returns the block tx results used to skip ante-failed
// txs in trace replays, or nil when they are unavailable.
func (b *Backend) blockTxResultsForTrace(height int64) []*abci.ExecTxResult {
	if blockRes := b.blockResultsForTrace(height); blockRes != nil {
		return blockRes.TxResults
	}
	return nil
}

// blockResultsForTrace returns the block results of a traced block, or nil
// when they are unavailable.
func (b *Backend) blockResultsForTrace(height int64) *cmrpctypes.ResultBlockResults {
	blockRes, err := b.TendermintBlockResultByNumber(&height)
	if err != nil || blockRes == nil {
		errMsg := "nil block results"
		if err != nil {
			errMsg = err.Error()
		}
		b.logger.Debug("block results unavailable for trace; ante-failed ethereum txs cannot be skipped", "height", height, "error", errMsg)
		return nil
	}
	return blockRes
}

func (b *Backend) populateTraceBlockTransactionHashes(
	results []*rpctypes.TxTraceResult,
	height int64,
	block *cmrpctypes.ResultBlock,
) error {
	if len(results) == 0 {
		return nil
	}

	allPresent := true
	for _, result := range results {
		if result == nil || result.TxHash == (common.Hash{}) {
			allPresent = false
			break
		}
	}
	if allPresent {
		return nil
	}

	if block == nil {
		var err error
		block, err = b.TendermintBlockByNumber(rpctypes.BlockNumber(height))
		if err != nil {
			return errors.Wrap(err, "block not found while populating trace transaction hashes")
		}
		if block == nil || block.Block == nil {
			return errors.New("block not found while populating trace transaction hashes")
		}
	}

	// Traces cached without hashes were produced by replaying every Ethereum
	// message of the block, so attach hashes against the unfiltered list.
	// Entries of txs that are not visible are dropped by the alignment.
	_, hashes := b.traceBlockEthereumTransactions(block, nil)
	return attachTraceBlockTransactionHashes(results, hashes)
}

func attachTraceBlockTransactionHashes(results []*rpctypes.TxTraceResult, hashes []common.Hash) error {
	if len(results) != len(hashes) {
		return fmt.Errorf("trace result count %d does not match Ethereum transaction count %d", len(results), len(hashes))
	}

	for i, hash := range hashes {
		if results[i] == nil {
			results[i] = &rpctypes.TxTraceResult{}
		}
		results[i].TxHash = hash
	}
	return nil
}

// traceUnavailableError is the error of a visible Ethereum tx without a
// trace entry, and of a tx whose synthetic trace cannot be built.
const traceUnavailableError = "transaction trace unavailable"

func traceBlockContextHeight(height rpctypes.BlockNumber, block *cmrpctypes.ResultBlock) int64 {
	traceHeight := int64(height)
	if block != nil && block.Block != nil && block.Block.Height > 0 {
		traceHeight = block.Block.Height
	}
	// Minus one to get the context at the beginning of the block.
	contextHeight := traceHeight - 1
	if contextHeight < 1 {
		// 0 is a special value for `ContextWithHeight`.
		return 1
	}
	return contextHeight
}

// TraceCall returns the structured logs created during the execution of EVM call
// and returns them as a JSON object.
func (b *Backend) TraceCall(
	args rpctypes.TransactionArgs, blockNrOrHash rpctypes.BlockNumberOrHash, config *rpctypes.TraceConfig,
) (interface{}, error) {
	config = b.normalizeTraceTimeout(config)
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	bz, err := sonic.Marshal(&args)
	if err != nil {
		return nil, err
	}
	blockNr, err := b.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	block, err := b.TendermintBlockByNumber(blockNr)
	if err != nil || block == nil {
		// the error message imitates geth behavior
		return nil, errors.New("header not found")
	}

	traceCallRequest := evmtypes.QueryTraceCallRequest{
		Args:            bz,
		GasCap:          b.RPCGasCap(),
		ProposerAddress: sdk.ConsAddress(block.Block.ProposerAddress),
		BlockNumber:     block.Block.Height,
		BlockHash:       common.Bytes2Hex(block.BlockID.Hash),
		BlockTime:       block.Block.Time,
		ChainId:         b.ChainID().ToInt().Int64(),
	}

	if config != nil {
		traceCallRequest.TraceConfig = b.convertConfig(config)
	}

	// get the context of provided block
	contextHeight := block.Block.Height
	if contextHeight < 1 {
		// 0 is a special value in `ContextWithHeight`
		contextHeight = 1
	}
	traceResult, err := b.queryClient.TraceCall(b.contextWithHeight(contextHeight), &traceCallRequest)
	if err != nil {
		return nil, err
	}

	// Response format is unknown due to custom tracer config param
	// More information can be found here https://geth.ethereum.org/docs/dapp/tracing-filtered
	var decodedResult interface{}
	err = sonic.Unmarshal(traceResult.Data, &decodedResult)
	if err != nil {
		return nil, err
	}

	return decodedResult, nil
}

func decodeCachedTraceTransaction(raw json.RawMessage) (interface{}, error) {
	var decoded interface{}
	if err := sonic.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func decodeCachedTraceBlock(raw json.RawMessage) ([]*rpctypes.TxTraceResult, error) {
	var decoded []*rpctypes.TxTraceResult
	if err := sonic.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		return []*rpctypes.TxTraceResult{}, nil
	}
	return decoded, nil
}

func (b *Backend) traceCacheHeight(height rpctypes.BlockNumber, block *cmrpctypes.ResultBlock) (int64, bool) {
	if block != nil && block.Block != nil {
		return block.Block.Height, true
	}
	if height > 0 {
		return height.Int64(), true
	}
	if !b.cfg.OfflineRPCOnly {
		return 0, false
	}
	h, err := b.indexedBlockHeight(height)
	if err != nil || h < 1 {
		return 0, false
	}
	return h, true
}

func (b *Backend) traceTransactionFromCachedBlock(hash common.Hash, config *rpctypes.TraceConfig) (interface{}, error) {
	if b.indexer == nil {
		return nil, errors.New("trace cache unavailable")
	}

	tx, err := b.GetTxByEthHash(hash)
	if err != nil {
		return nil, err
	}

	raw, err := b.indexer.GetTraceBlockByHeight(tx.Height, config)
	if err != nil {
		return nil, err
	}

	blockTrace, err := decodeCachedTraceBlock(raw)
	if err != nil {
		return nil, err
	}
	if err := b.populateTraceBlockTransactionHashes(blockTrace, tx.Height, nil); err != nil {
		return nil, err
	}
	blockTrace = b.alignTraceResults(blockTrace, b.traceBlockView(tx.Height, nil, nil), config)

	// Look the entry up by hash: the aligned trace follows the visible tx order,
	// which differs from the Ethereum tx index when virtual txs are present.
	for _, entry := range blockTrace {
		if entry == nil || entry.TxHash != hash {
			continue
		}
		if entry.Error != "" {
			return nil, errors.New(entry.Error)
		}
		return entry.Result, nil
	}
	return nil, errors.New("transaction trace not found in cached block trace")
}
