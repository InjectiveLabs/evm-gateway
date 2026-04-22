package backend

import (
	"fmt"

	errorsmod "cosmossdk.io/errors"
	abci "github.com/cometbft/cometbft/abci/types"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

// GetTxHashByEthHash returns BFT tx hash by eth tx hash
func (b *Backend) GetTxHashByEthHash(ethHash common.Hash) (common.Hash, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	res, err := b.GetTxByEthHash(ethHash)
	if err != nil {
		return common.Hash{}, err
	}

	block, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(res.Height))
	if err != nil {
		return common.Hash{}, errors.Wrap(err, "block not found")
	}
	if block == nil {
		return common.Hash{}, errors.New("block not found")
	}

	bftHash := block.Block.Txs[res.TxIndex].Hash()

	return common.Hash(bftHash), nil
}

// GetTransactionByHash returns the Ethereum format transaction identified by Ethereum transaction hash
func (b *Backend) GetTransactionByHash(txHash common.Hash) (*rpctypes.RPCTransaction, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		rpcTx, err := b.indexer.GetRPCTransactionByHash(txHash)
		if err == nil {
			visible, err := b.cachedTransactionVisible(txHash)
			if err != nil {
				return nil, err
			}
			if !visible {
				return nil, nil
			}
			matches, err := b.cachedRPCTransactionMatchesVirtualization(rpcTx)
			if err != nil {
				return nil, err
			}
			if !matches {
				if b.cfg.OfflineRPCOnly {
					return nil, nil
				}
			} else {
				if b.syncStatus != nil {
					b.syncStatus.RecordTxByHashCacheHit()
				}
				return rpcTx, nil
			}
		}
	}

	res, err := b.GetTxByEthHash(txHash)
	if err != nil {
		if b.cfg.OfflineRPCOnly {
			b.logger.Debug("tx not found in offline cache", "hash", txHash.Hex(), "error", err.Error())
			return nil, nil
		}
		return b.getTransactionByHashPending(txHash)
	}

	block, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(res.Height))
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}

	tx, err := b.clientCtx.TxConfig.TxDecoder()(block.Block.Txs[res.TxIndex])
	if err != nil {
		return nil, err
	}

	// the `res.MsgIndex` is inferred from tx index, should be within the bound.
	msg, ok := tx.GetMsgs()[res.MsgIndex].(*evmtypes.MsgEthereumTx)
	if !ok {
		return nil, errors.New("invalid ethereum tx")
	}

	blockRes, err := b.TendermintBlockResultByNumber(&block.Block.Height)
	if err != nil {
		b.logger.Debug("block result not found", "height", block.Block.Height, "error", err.Error())
		return nil, nil
	}

	if b.virtualBankEnabled() {
		view, err := b.liveVirtualBankBlockView(block, blockRes)
		if err != nil {
			return nil, err
		}
		for _, rpcTx := range view.Transactions {
			if rpcTx != nil && rpcTx.Hash == txHash {
				return rpcTx, nil
			}
		}
		return nil, nil
	}

	if res.EthTxIndex == -1 {
		// Fallback to find tx index by iterating all valid eth transactions
		msgs := b.EthMsgsFromTendermintBlock(block)
		for i := range msgs {
			if msgs[i].Hash() == txHash {
				res.EthTxIndex = int32(i)
				break
			}
		}
	}
	// if we still unable to find the eth tx index, return error, shouldn't happen.
	if res.EthTxIndex == -1 {
		return nil, errors.New("can't find index of ethereum tx")
	}

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle the error for pruned node.
		b.logger.Error("failed to fetch Base Fee from prunned block. Check node prunning configuration", "height", blockRes.Height, "error", err)
	}

	return rpctypes.NewTransactionFromMsg(
		msg,
		common.BytesToHash(block.BlockID.Hash.Bytes()),
		uint64(res.Height),
		uint64(res.EthTxIndex),
		baseFee,
		b.ChainID().ToInt(),
	)
}

// getTransactionByHashPending find pending tx from mempool
func (b *Backend) getTransactionByHashPending(txHash common.Hash) (*rpctypes.RPCTransaction, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	// try to find tx in mempool
	txs, err := b.PendingTransactions()
	if err != nil {
		b.logger.Debug("tx not found", "hash", txHash, "error", err.Error())
		return nil, nil
	}

	for _, tx := range txs {
		msg, err := evmtypes.UnwrapEthereumMsg(tx, txHash)
		if err != nil {
			// not ethereum tx
			continue
		}

		if msg.Hash() == txHash {
			// use zero block values since it's not included in a block yet
			rpctx, err := rpctypes.NewTransactionFromMsg(
				msg,
				common.Hash{},
				uint64(0),
				uint64(0),
				nil,
				b.ChainID().ToInt(),
			)
			if err != nil {
				return nil, err
			}
			return rpctx, nil
		}
	}

	b.logger.Debug("tx not found", "hash", txHash)
	return nil, nil
}

// GetGasUsed returns gasUsed from transaction
func (b *Backend) GetGasUsed(res *chaintypes.TxResult, gas uint64) uint64 {
	return res.GasUsed
}

// GetTransactionReceipt returns the transaction receipt identified by hash.
func (b *Backend) GetTransactionReceipt(hash common.Hash) (map[string]interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	b.logger.Debug("eth_getTransactionReceipt", "hash", hash)
	if b.indexer != nil {
		receipt, err := b.materializedReceiptByHash(hash)
		if err == nil {
			visible, err := b.cachedTransactionVisible(hash)
			if err != nil {
				return nil, err
			}
			if !visible {
				return nil, nil
			}
			matches, err := b.cachedReceiptMatchesVirtualization(receipt)
			if err != nil {
				return nil, err
			}
			if !matches {
				if b.cfg.OfflineRPCOnly {
					return nil, nil
				}
			} else {
				if b.syncStatus != nil {
					b.syncStatus.RecordReceiptCacheHit()
				}
				return receipt, nil
			}
		}
		if b.syncStatus != nil {
			b.syncStatus.RecordReceiptCacheMiss()
			b.syncStatus.RecordReceiptLiveFallback()
		}
	}

	res, err := b.GetTxByEthHash(hash)
	if err != nil {
		b.logger.Debug("tx not found", "hash", hash, "error", err.Error())
		if b.cfg.OfflineRPCOnly {
			return nil, nil
		}
		return nil, nil
	}
	resBlock, err := b.TendermintBlockByNumber(rpctypes.BlockNumber(res.Height))
	if err != nil {
		b.logger.Debug("block not found", "height", res.Height, "error", err.Error())
		return nil, err
	}
	if resBlock == nil {
		b.logger.Debug("block not found", "height", res.Height)
		return nil, nil
	}

	tx, err := b.clientCtx.TxConfig.TxDecoder()(resBlock.Block.Txs[res.TxIndex])
	if err != nil {
		b.logger.Warn("decoding failed", "error", err.Error())
		return nil, errors.Wrap(err, "failed to decode tx")
	}
	ethMsg := tx.GetMsgs()[res.MsgIndex].(*evmtypes.MsgEthereumTx)

	txData := ethMsg.AsTransaction()
	if txData == nil {
		b.logger.Error("failed to unpack tx data")
		return nil, err
	}

	cumulativeGasUsed := uint64(0)
	blockRes, err := b.TendermintBlockResultByNumber(&res.Height)
	if err != nil {
		b.logger.Warn("failed to retrieve block results", "height", res.Height, "error", err.Error())
		return nil, nil
	}
	if b.virtualBankEnabled() {
		view, err := b.liveVirtualBankBlockView(resBlock, blockRes)
		if err != nil {
			return nil, err
		}
		for _, receipt := range view.Receipts {
			if receiptHash, ok := receipt["transactionHash"].(common.Hash); ok && receiptHash == hash {
				return receipt, nil
			}
		}
		return nil, nil
	}
	normalizedTxResults, err := rpctypes.NormalizeTxResponseIndexes(blockRes.TxResults)
	if err != nil {
		b.logger.Warn("failed to normalize tx response indexes", "height", res.Height, "error", err.Error())
		normalizedTxResults = blockRes.TxResults
	}
	for _, txResult := range blockRes.TxResults[0:res.TxIndex] {
		cumulativeGasUsed += uint64(txResult.GasUsed)
	}
	cumulativeGasUsed += res.CumulativeGasUsed

	var status hexutil.Uint
	if res.Failed {
		status = hexutil.Uint(ethtypes.ReceiptStatusFailed)
	} else {
		status = hexutil.Uint(ethtypes.ReceiptStatusSuccessful)
	}

	from, err := ethMsg.GetSenderLegacy(ethtypes.LatestSignerForChainID(b.ChainID().ToInt()))
	if err != nil {
		return nil, err
	}

	// parse tx logs from events
	logs, err := evmtypes.DecodeMsgLogs(
		normalizedTxResults[res.TxIndex].Data,
		int(res.MsgIndex),
		uint64(blockRes.Height),
	)
	if err != nil {
		b.logger.Warn("failed to parse logs", "hash", hash, "error", err.Error())
	}

	if res.EthTxIndex == -1 {
		// Fallback to find tx index by iterating all valid eth transactions
		msgs := b.EthMsgsFromTendermintBlock(resBlock)
		for i := range msgs {
			if msgs[i].Hash() == hash {
				res.EthTxIndex = int32(i)
				break
			}
		}
	}
	// return error if still unable to find the eth tx index
	if res.EthTxIndex == -1 {
		return nil, errors.New("can't find index of ethereum tx")
	}

	receipt := map[string]interface{}{
		// Consensus fields: These fields are defined by the Yellow Paper
		"status":            status,
		"cumulativeGasUsed": hexutil.Uint64(cumulativeGasUsed),
		"logsBloom":         ethtypes.BytesToBloom(evmtypes.LogsBloom(logs)),
		"logs":              logs,

		// Implementation fields: These fields are added by geth when processing a transaction.
		// They are stored in the chain database.
		"transactionHash": hash,
		"contractAddress": nil,
		"gasUsed":         hexutil.Uint64(b.GetGasUsed(res, txData.Gas())),

		// Inclusion information: These fields provide information about the inclusion of the
		// transaction corresponding to this receipt.
		"blockHash":        common.BytesToHash(resBlock.Block.Header.Hash()).Hex(),
		"blockNumber":      hexutil.Uint64(res.Height),
		"transactionIndex": hexutil.Uint64(res.EthTxIndex),

		// https://github.com/foundry-rs/foundry/issues/7640
		"effectiveGasPrice": (*hexutil.Big)(txData.GasPrice()),

		// sender and receiver (contract or EOA) addreses
		"from": from,
		"to":   txData.To(),
		"type": hexutil.Uint(ethMsg.AsTransaction().Type()),
	}

	if logs == nil {
		receipt["logs"] = [][]*ethtypes.Log{}
	}

	// If the ContractAddress is 20 0x0 bytes, assume it is not a contract creation
	if txData.To() == nil {
		receipt["contractAddress"] = crypto.CreateAddress(from, txData.Nonce())
	}

	if txData.Type() == ethtypes.DynamicFeeTxType {
		tx := ethMsg.AsTransaction()
		price := tx.GasPrice()
		receipt["effectiveGasPrice"] = hexutil.Big(*price)
	}

	return receipt, nil
}

// GetTransactionByBlockHashAndIndex returns the transaction identified by hash and index.
func (b *Backend) GetTransactionByBlockHashAndIndex(hash common.Hash, idx hexutil.Uint) (*rpctypes.RPCTransaction, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	b.logger.Debug("eth_getTransactionByBlockHashAndIndex", "hash", hash.Hex(), "index", idx)

	if b.indexer != nil {
		meta, err := b.indexer.GetBlockMetaByHash(hash)
		if err == nil {
			if !b.cachedMetaMatchesVirtualization(meta) {
				if b.cfg.OfflineRPCOnly {
					return nil, fmt.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
				}
				b.logger.Debug("cached block virtualization mode mismatch; falling back to live rpc", "hash", hash.Hex(), "height", meta.Height)
			} else {
				rpcTx, err := b.indexer.GetRPCTransactionByBlockAndIndex(meta.Height, int32(idx))
				if err == nil {
					if b.syncStatus != nil {
						b.syncStatus.RecordTxByIndexCacheHit()
					}
					return rpcTx, nil
				}
				if b.syncStatus != nil {
					b.syncStatus.RecordTxByIndexCacheMiss()
				}
				if b.cfg.OfflineRPCOnly {
					if isIndexerCacheMiss(err) {
						return nil, nil
					}
					return nil, err
				}
				if !isIndexerCacheMiss(err) {
					b.logger.Debug("cached tx-by-block-hash lookup failed; falling back to live rpc", "hash", hash.Hex(), "index", idx, "error", err.Error())
				}
			}
		} else if b.cfg.OfflineRPCOnly {
			if isIndexerCacheMiss(err) {
				return nil, nil
			}
			return nil, err
		} else if !isIndexerCacheMiss(err) {
			b.logger.Debug("cached block meta lookup by hash failed; falling back to live rpc", "hash", hash.Hex(), "error", err.Error())
		}
	}

	sc, ok := b.clientCtx.Client.(cmrpcclient.SignClient)
	if !ok {
		return nil, errors.New("invalid rpc client")
	}

	block, err := sc.BlockByHash(b.ctx, hash.Bytes())
	if err != nil {
		b.logger.Debug("block not found", "hash", hash.Hex(), "error", err.Error())
		return nil, nil
	}

	if block.Block == nil {
		b.logger.Debug("block not found", "hash", hash.Hex())
		return nil, nil
	}

	return b.GetTransactionByBlockAndIndex(block, idx)
}

// GetTransactionByBlockNumberAndIndex returns the transaction identified by number and index.
func (b *Backend) GetTransactionByBlockNumberAndIndex(blockNum rpctypes.BlockNumber, idx hexutil.Uint) (*rpctypes.RPCTransaction, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	b.logger.Debug("eth_getTransactionByBlockNumberAndIndex", "number", blockNum, "index", idx)

	if b.indexer != nil {
		height, err := b.indexedBlockHeight(blockNum)
		if err == nil && height >= 1 {
			meta, metaErr := b.indexer.GetBlockMetaByHeight(height)
			if metaErr == nil && meta != nil && !b.cachedMetaMatchesVirtualization(meta) {
				if b.cfg.OfflineRPCOnly {
					return nil, fmt.Errorf("cached block virtualization mode mismatch at height %d", meta.Height)
				}
				b.logger.Debug("cached block virtualization mode mismatch; falling back to live rpc", "height", meta.Height)
			} else {
				rpcTx, err := b.indexer.GetRPCTransactionByBlockAndIndex(height, int32(idx))
				if err == nil {
					if b.syncStatus != nil {
						b.syncStatus.RecordTxByIndexCacheHit()
					}
					return rpcTx, nil
				}
				if b.syncStatus != nil {
					b.syncStatus.RecordTxByIndexCacheMiss()
				}
				if b.cfg.OfflineRPCOnly {
					if isIndexerCacheMiss(err) {
						return nil, nil
					}
					return nil, err
				}
				if !isIndexerCacheMiss(err) {
					b.logger.Debug("cached tx-by-block-number lookup failed; falling back to live rpc", "height", height, "index", idx, "error", err.Error())
				}
			}
		} else if b.cfg.OfflineRPCOnly {
			if isIndexerCacheMiss(err) {
				return nil, nil
			}
			return nil, err
		} else if err != nil && !isIndexerCacheMiss(err) {
			b.logger.Debug("cached block height lookup failed; falling back to live rpc", "number", blockNum, "error", err.Error())
		}
	}

	block, err := b.TendermintBlockByNumber(blockNum)
	if err != nil {
		b.logger.Debug("block not found", "height", blockNum.Int64(), "error", err.Error())
		return nil, err
	}

	if block == nil || block.Block == nil {
		b.logger.Debug("block not found", "height", blockNum.Int64())
		return nil, nil
	}

	return b.GetTransactionByBlockAndIndex(block, idx)
}

// GetTxByEthHash uses `/tx_query` to find transaction by ethereum tx hash
// TODO: Don't need to convert once hashing is fixed on Tendermint
// https://github.com/tendermint/tendermint/issues/6539
func (b *Backend) GetTxByEthHash(hash common.Hash) (*chaintypes.TxResult, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		res, err := b.indexer.GetByTxHash(hash)
		if err == nil {
			if b.syncStatus != nil {
				b.syncStatus.RecordTxByHashCacheHit()
			}
			return res, nil
		}
		if b.syncStatus != nil {
			b.syncStatus.RecordTxByHashCacheMiss()
		}
		b.logger.Debug("tx cache miss; fallback to live query", "tx", hash.Hex(), "error", err.Error())
	}
	if b.syncStatus != nil {
		b.syncStatus.RecordTxByHashLiveFallback()
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.New("ethereum tx not found in offline cache")
	}

	// fallback to tendermint tx indexer
	b.logger.Warn("fallback to tendermint tx indexer! failed txns will not be available", "tx", hash.Hex())
	query := fmt.Sprintf("%s.%s='%s'", evmtypes.TypeMsgEthereumTx, evmtypes.AttributeKeyEthereumTxHash, hash.Hex())
	txResult, err := b.queryTendermintTxIndexer(query, func(txs *rpctypes.ParsedTxs) *rpctypes.ParsedTx {
		return txs.GetTxByHash(hash)
	})
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetTxByEthHash %s", hash.Hex())
	}
	return txResult, nil
}

// GetTxByTxIndex uses `/tx_query` to find transaction by tx index of valid ethereum txs
func (b *Backend) GetTxByTxIndex(height int64, index uint) (*chaintypes.TxResult, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		res, err := b.indexer.GetByBlockAndIndex(height, int32(index))
		if err == nil {
			if b.syncStatus != nil {
				b.syncStatus.RecordTxByIndexCacheHit()
			}
			return res, nil
		}
		if b.syncStatus != nil {
			b.syncStatus.RecordTxByIndexCacheMiss()
		}
		b.logger.Debug("tx-index cache miss; fallback to live query", "height", height, "index", index, "error", err.Error())
	}
	if b.syncStatus != nil {
		b.syncStatus.RecordTxByIndexLiveFallback()
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.New("ethereum tx not found in offline cache")
	}

	// fallback to tendermint tx indexer
	b.logger.Warn("fallback to tendermint tx indexer! failed txns will not be available", "height", height, "txIndex", index)
	query := fmt.Sprintf("tx.height=%d AND %s.%s=%d",
		height, evmtypes.TypeMsgEthereumTx,
		evmtypes.AttributeKeyTxIndex, index,
	)
	txResult, err := b.queryTendermintTxIndexer(query, func(txs *rpctypes.ParsedTxs) *rpctypes.ParsedTx {
		return txs.GetTxByTxIndex(int(index))
	})
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetTxByTxIndex %d %d", height, index)
	}
	return txResult, nil
}

// queryTendermintTxIndexer query tx in tendermint tx indexer
func (b *Backend) queryTendermintTxIndexer(query string, txGetter func(*rpctypes.ParsedTxs) *rpctypes.ParsedTx) (*chaintypes.TxResult, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.clientCtx.Client == nil {
		return nil, errors.New("tendermint tx indexer unavailable without rpc client")
	}

	resTxs, err := b.clientCtx.Client.TxSearch(b.ctx, query, false, nil, nil, "")
	if err != nil {
		return nil, err
	}

	if len(resTxs.Txs) == 0 {
		return nil, errors.New("ethereum tx not found")
	}

	txResult := resTxs.Txs[0]

	var tx sdk.Tx
	if rpctypes.TxSuccessOrExceedsBlockGasLimit(&txResult.TxResult) &&
		txResult.TxResult.Code != abci.CodeTypeOK &&
		txResult.TxResult.Codespace != evmtypes.ModuleName {

		// only needed when the tx exceeds block gas limit
		tx, err = b.clientCtx.TxConfig.TxDecoder()(txResult.Tx)
		if err != nil {
			return nil, fmt.Errorf("invalid ethereum tx")
		}
	}

	return rpctypes.ParseTxIndexerResult(txResult, tx, txGetter)
}

// GetTransactionByBlockAndIndex is the common code shared by `GetTransactionByBlockNumberAndIndex` and `GetTransactionByBlockHashAndIndex`.
func (b *Backend) GetTransactionByBlockAndIndex(block *cmrpctypes.ResultBlock, idx hexutil.Uint) (*rpctypes.RPCTransaction, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.indexer != nil {
		rpcTx, err := b.indexer.GetRPCTransactionByBlockAndIndex(block.Block.Height, int32(idx))
		if err == nil {
			if b.syncStatus != nil {
				b.syncStatus.RecordTxByIndexCacheHit()
			}
			return rpcTx, nil
		}
	}

	blockRes, err := b.TendermintBlockResultByNumber(&block.Block.Height)
	if err != nil {
		return nil, nil
	}

	if b.virtualBankEnabled() {
		view, err := b.liveVirtualBankBlockView(block, blockRes)
		if err != nil {
			return nil, err
		}
		i := int(idx)
		if i < 0 || i >= len(view.Transactions) {
			b.logger.Warn("block txs index out of bound", "index", i)
			return nil, nil
		}
		return view.Transactions[i], nil
	}

	var msg *evmtypes.MsgEthereumTx
	// find in tx indexer
	res, err := b.GetTxByTxIndex(block.Block.Height, uint(idx))
	if err == nil {
		tx, err := b.clientCtx.TxConfig.TxDecoder()(block.Block.Txs[res.TxIndex])
		if err != nil {
			b.logger.Warn("invalid ethereum tx", "height", block.Block.Header, "index", idx)
			return nil, nil
		}

		var ok bool
		// msgIndex is inferred from tx events, should be within bound.
		msg, ok = tx.GetMsgs()[res.MsgIndex].(*evmtypes.MsgEthereumTx)
		if !ok {
			b.logger.Warn("invalid ethereum tx", "height", block.Block.Header, "index", idx)
			return nil, nil
		}
	} else {
		i := int(idx)
		if i < 0 {
			i = 0
		}
		ethMsgs := b.EthMsgsFromTendermintBlock(block)
		if i >= len(ethMsgs) {
			b.logger.Warn("block txs index out of bound", "index", i)
			return nil, nil
		}

		msg = ethMsgs[i]
	}

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle the error for pruned node.
		b.logger.Error("failed to fetch Base Fee from prunned block. Check node prunning configuration", "height", block.Block.Height, "error", err)
	}

	return rpctypes.NewTransactionFromMsg(
		msg,
		common.BytesToHash(block.Block.Hash()),
		uint64(block.Block.Height),
		uint64(idx),
		baseFee,
		b.ChainID().ToInt(),
	)
}
