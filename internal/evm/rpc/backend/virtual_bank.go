package backend

import (
	"fmt"
	"math/big"

	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

type liveVirtualBankBlockView struct {
	Transactions []*rpctypes.RPCTransaction
	Receipts     []map[string]interface{}
	Logs         [][]*virtualbank.RPCLog
}

type virtualRPCTransactionLookup interface {
	IsVirtualRPCTransaction(hash common.Hash) (bool, error)
}

// virtualBankEnabled reports whether live RPC paths should synthesize Cosmos
// x/bank events into Ethereum-compatible logs, receipts, and transactions.
func (b *Backend) virtualBankEnabled() bool {
	return b.cfg.VirtualizeCosmosEvents
}

// cachedTransactionVisible reports whether an indexed RPC transaction should be
// exposed under the current virtualization setting. Virtual-only transactions
// are hidden when the backend is running in non-virtualized mode.
func (b *Backend) cachedTransactionVisible(hash common.Hash) (bool, error) {
	if b.virtualBankEnabled() || b.indexer == nil {
		return true, nil
	}
	lookup, ok := b.indexer.(virtualRPCTransactionLookup)
	if !ok {
		return true, nil
	}
	isVirtual, err := lookup.IsVirtualRPCTransaction(hash)
	if err != nil {
		return false, err
	}
	return !isVirtual, nil
}

// cachedRPCTransactionMatchesVirtualization checks that an indexed transaction's
// block was cached with the same virtualized/non-virtualized mode as this RPC
// backend.
func (b *Backend) cachedRPCTransactionMatchesVirtualization(tx *rpctypes.RPCTransaction) (bool, error) {
	if tx == nil || tx.BlockNumber == nil || b.indexer == nil {
		return true, nil
	}
	height := (*big.Int)(tx.BlockNumber).Int64()
	meta, err := b.indexer.GetBlockMetaByHeight(height)
	if err != nil {
		if isIndexerCacheMiss(err) {
			return true, nil
		}
		return false, err
	}
	return b.cachedMetaMatchesVirtualization(meta), nil
}

// cachedReceiptMatchesVirtualization checks that an indexed receipt belongs to
// a block cached with the current virtualization mode before it is served.
func (b *Backend) cachedReceiptMatchesVirtualization(receipt map[string]interface{}) (bool, error) {
	if receipt == nil || b.indexer == nil {
		return true, nil
	}
	raw, ok := receipt["blockNumber"]
	if !ok {
		return true, nil
	}

	var height int64
	switch v := raw.(type) {
	case hexutil.Uint64:
		height = int64(v)
	case uint64:
		height = int64(v)
	case *hexutil.Big:
		height = (*big.Int)(v).Int64()
	default:
		return true, nil
	}

	meta, err := b.indexer.GetBlockMetaByHeight(height)
	if err != nil {
		if isIndexerCacheMiss(err) {
			return true, nil
		}
		return false, err
	}
	return b.cachedMetaMatchesVirtualization(meta), nil
}

// liveVirtualBankBlockView builds the RPC-visible block view directly from live
// Comet block data when Cosmos event virtualization is enabled. It merges native
// EVM logs with synthesized x/bank logs and creates virtual transactions for
// non-EVM Cosmos messages and begin/end block events.
func (b *Backend) liveVirtualBankBlockView(
	resBlock *cmrpctypes.ResultBlock,
	blockRes *cmrpctypes.ResultBlockResults,
) (*liveVirtualBankBlockView, error) {
	if resBlock == nil || resBlock.Block == nil {
		return nil, fmt.Errorf("tendermint block is nil")
	}
	if blockRes == nil {
		return nil, fmt.Errorf("tendermint block result is nil")
	}

	block := resBlock.Block
	blockHash := common.BytesToHash(block.Hash())
	blockHashHex := blockHash.Hex()
	blockNumber := uint64(block.Height)

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		b.logger.Debug("failed to query base fee for virtualized live block", "height", block.Height, "error", err.Error())
	}

	normalizedTxResults, err := rpctypes.NormalizeTxResponseIndexes(blockRes.TxResults)
	if err != nil {
		return nil, err
	}

	view := &liveVirtualBankBlockView{
		Transactions: make([]*rpctypes.RPCTransaction, 0),
		Receipts:     make([]map[string]interface{}, 0),
		Logs:         make([][]*virtualbank.RPCLog, 0),
	}

	var (
		logIndex                   uint
		rpcTxIndex                 uint64
		blockResultGasUsedBeforeTx uint64
	)

	virtualLogContext := func(txHash common.Hash, txIndex uint64) virtualbank.LogContext {
		return virtualbank.LogContext{
			BlockHash:     blockHash,
			BlockNumber:   blockNumber,
			TxHash:        txHash,
			TxIndex:       uint(txIndex),
			FirstLogIndex: logIndex,
		}
	}
	appendLogs := func(logs []*virtualbank.RPCLog) {
		view.Logs = append(view.Logs, logs)
	}
	appendVirtualTx := func(txHash common.Hash, txIndex uint64, logs []*virtualbank.RPCLog, status uint64, gasUsed uint64, cumulativeGasUsed uint64, cosmosHash *common.Hash) {
		rpcTx := virtualbank.NewRPCTransaction(txHash, blockHash, blockNumber, txIndex, b.ChainID().ToInt(), cosmosHash)
		view.Transactions = append(view.Transactions, rpcTx)
		view.Receipts = append(view.Receipts, virtualReceiptMap(status, cumulativeGasUsed, gasUsed, logs, txHash, blockHashHex, blockNumber, txIndex))
		appendLogs(logs)
	}

	beginBlockEvents, endBlockEvents, err := virtualbank.SplitBlockEvents(blockRes.FinalizeBlockEvents)
	if err != nil {
		return nil, err
	}
	if len(beginBlockEvents) > 0 {
		txHash := virtualbank.BeginBlockHash(block.Height)
		logs, err := virtualbank.Logs(beginBlockEvents, virtualLogContext(txHash, rpcTxIndex))
		if err != nil {
			return nil, err
		}
		logIndex += uint(len(logs))
		appendVirtualTx(txHash, rpcTxIndex, logs, uint64(ethtypes.ReceiptStatusSuccessful), 0, 0, nil)
		rpcTxIndex++
	}

	for txIndex, txBz := range block.Txs {
		if txIndex >= len(normalizedTxResults) {
			return nil, fmt.Errorf("block results shorter than tx list at height %d", block.Height)
		}
		txResult := normalizedTxResults[txIndex]
		if txResult == nil {
			return nil, fmt.Errorf("missing tx result at height %d index %d", block.Height, txIndex)
		}
		resultGasUsed := uint64(txResult.GasUsed)

		tx, err := b.clientCtx.TxConfig.TxDecoder()(txBz)
		if err != nil {
			b.logger.Warn("failed to decode transaction in block", "height", block.Height, "txIndex", txIndex, "error", err.Error())
			blockResultGasUsedBeforeTx += resultGasUsed
			continue
		}

		msgs := tx.GetMsgs()
		ethMsgIndexes := make(map[int]bool)
		for msgIndex, msg := range msgs {
			if _, ok := msg.(*evmtypes.MsgEthereumTx); ok {
				ethMsgIndexes[msgIndex] = true
			}
		}

		bankEvents, err := virtualbank.ParseEvents(txResult.Events)
		if err != nil {
			return nil, err
		}

		parsedTxs, parsedErr := rpctypes.ParseTxResult(txResult, tx)
		if parsedErr != nil && (txResult.Code == abci.CodeTypeOK || txResult.Codespace == evmtypes.ModuleName) && len(ethMsgIndexes) > 0 {
			b.logger.Warn("failed to parse tx result", "height", block.Height, "txIndex", txIndex, "error", parsedErr.Error())
		}

		var cumulativeTxEthGasUsed uint64
		for msgIndex, msg := range msgs {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				continue
			}

			txData := ethMsg.AsTransaction()
			if txData == nil {
				return nil, fmt.Errorf("failed to unpack tx data")
			}

			txHash := ethMsg.Hash()
			txFailed := false
			txGasUsed := ethMsg.GetGas()
			if txResult.Code != abci.CodeTypeOK && txResult.Codespace != evmtypes.ModuleName {
				txFailed = true
			} else if parsedTxs != nil {
				parsedTx := parsedTxs.GetTxByMsgIndex(msgIndex)
				if parsedTx == nil {
					b.logger.Warn("msg index not found in parsed tx result", "height", block.Height, "txIndex", txIndex, "msgIndex", msgIndex)
					txFailed = txResult.Code != abci.CodeTypeOK
				} else {
					txHash = parsedTx.Hash
					txGasUsed = parsedTx.GasUsed
					txFailed = parsedTx.Failed
				}
			}

			cumulativeTxEthGasUsed += txGasUsed

			evmLogs, err := evmtypes.DecodeMsgLogs(txResult.Data, msgIndex, blockNumber)
			if err != nil {
				b.logger.Warn("failed to parse logs", "hash", txHash, "error", err.Error())
			}
			logs := virtualbank.WrapLogs(evmLogs, false, nil)
			virtualbank.SetLogMetadata(logs, virtualLogContext(txHash, rpcTxIndex))
			logIndex += uint(len(logs))

			events := virtualbank.EventsForMsg(bankEvents, msgIndex, len(msgs))
			if len(events) > 0 {
				virtualLogs, err := virtualbank.Logs(events, virtualLogContext(txHash, rpcTxIndex))
				if err != nil {
					return nil, err
				}
				logIndex += uint(len(virtualLogs))
				logs = append(logs, virtualLogs...)
			}

			status := uint64(ethtypes.ReceiptStatusSuccessful)
			if txFailed {
				status = uint64(ethtypes.ReceiptStatusFailed)
			}

			var signer ethtypes.Signer
			if txData.Protected() {
				signer = ethtypes.LatestSignerForChainID(txData.ChainId())
			} else {
				signer = ethtypes.FrontierSigner{}
			}
			from, err := ethMsg.GetSenderLegacy(signer)
			if err != nil {
				return nil, err
			}

			var contractAddress *common.Address
			if txData.To() == nil {
				addr := crypto.CreateAddress(from, txData.Nonce())
				contractAddress = &addr
			}

			rpcTx, err := rpctypes.NewRPCTransaction(ethMsg, blockHash, blockNumber, rpcTxIndex, baseFee, b.ChainID().ToInt())
			if err != nil {
				return nil, err
			}
			rpcTx.Hash = txHash
			view.Transactions = append(view.Transactions, rpcTx)
			view.Receipts = append(view.Receipts, liveReceiptMap(
				status,
				blockResultGasUsedBeforeTx+cumulativeTxEthGasUsed,
				txGasUsed,
				logs,
				txHash,
				contractAddress,
				blockHashHex,
				blockNumber,
				rpcTxIndex,
				txData.GasPrice(),
				from,
				txData.To(),
				uint64(txData.Type()),
			))
			appendLogs(logs)

			rpcTxIndex++
		}

		events := virtualbank.EventsForNonEthMessages(bankEvents, ethMsgIndexes, len(msgs))
		if len(events) > 0 {
			cosmosHash := virtualbank.OriginalCosmosTxHash(txBz)
			txHash := virtualbank.CosmosTxHash(txBz)
			ctx := virtualLogContext(txHash, rpcTxIndex)
			ctx.CosmosHash = &cosmosHash
			logs, err := virtualbank.Logs(events, ctx)
			if err != nil {
				return nil, err
			}
			logIndex += uint(len(logs))

			status := uint64(ethtypes.ReceiptStatusSuccessful)
			if txResult.Code != abci.CodeTypeOK {
				status = uint64(ethtypes.ReceiptStatusFailed)
			}
			appendVirtualTx(txHash, rpcTxIndex, logs, status, resultGasUsed, blockResultGasUsedBeforeTx+resultGasUsed, &cosmosHash)
			rpcTxIndex++
		}

		blockResultGasUsedBeforeTx += resultGasUsed
	}

	if len(endBlockEvents) > 0 {
		txHash := virtualbank.EndBlockHash(block.Height)
		logs, err := virtualbank.Logs(endBlockEvents, virtualLogContext(txHash, rpcTxIndex))
		if err != nil {
			return nil, err
		}
		logIndex += uint(len(logs))
		appendVirtualTx(txHash, rpcTxIndex, logs, uint64(ethtypes.ReceiptStatusSuccessful), 0, blockResultGasUsedBeforeTx, nil)
	}

	return view, nil
}

// liveReceiptMap builds an Ethereum receipt map from live block result data,
// preserving native EVM transaction fields and any merged virtual logs.
func liveReceiptMap(
	status uint64,
	cumulativeGasUsed uint64,
	gasUsed uint64,
	logs []*virtualbank.RPCLog,
	txHash common.Hash,
	contractAddress *common.Address,
	blockHash string,
	blockNumber uint64,
	transactionIndex uint64,
	effectiveGasPrice *big.Int,
	from common.Address,
	to *common.Address,
	txType uint64,
) map[string]interface{} {
	receipt := map[string]interface{}{
		"status":            hexutil.Uint(status),
		"cumulativeGasUsed": hexutil.Uint64(cumulativeGasUsed),
		"logsBloom":         ethtypes.BytesToBloom(evmtypes.LogsBloom(virtualbank.EthLogs(logs))),
		"logs":              logs,
		"transactionHash":   txHash,
		"contractAddress":   nil,
		"gasUsed":           hexutil.Uint64(gasUsed),
		"blockHash":         blockHash,
		"blockNumber":       hexutil.Uint64(blockNumber),
		"transactionIndex":  hexutil.Uint64(transactionIndex),
		"effectiveGasPrice": (*hexutil.Big)(effectiveGasPrice),
		"from":              from,
		"to":                to,
		"type":              hexutil.Uint(txType),
	}
	if logs == nil {
		receipt["logs"] = []*virtualbank.RPCLog{}
	}
	if contractAddress != nil {
		receipt["contractAddress"] = *contractAddress
	}
	return receipt
}

// virtualReceiptMap builds the receipt shape for synthesized virtual bank
// transactions that have no underlying EVM transaction.
func virtualReceiptMap(
	status uint64,
	cumulativeGasUsed uint64,
	gasUsed uint64,
	logs []*virtualbank.RPCLog,
	txHash common.Hash,
	blockHash string,
	blockNumber uint64,
	transactionIndex uint64,
) map[string]interface{} {
	return liveReceiptMap(
		status,
		cumulativeGasUsed,
		gasUsed,
		logs,
		txHash,
		nil,
		blockHash,
		blockNumber,
		transactionIndex,
		big.NewInt(0),
		common.Address{},
		&virtualbank.ContractAddress,
		uint64(ethtypes.LegacyTxType),
	)
}
