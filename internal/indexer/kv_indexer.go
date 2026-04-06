package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"

	errorsmod "cosmossdk.io/errors"
	abci "github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authante "github.com/cosmos/cosmos-sdk/x/auth/ante"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	txfeestypes "github.com/InjectiveLabs/sdk-go/chain/txfees/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

const (
	KeyPrefixTxHash  = 1
	KeyPrefixTxIndex = 2

	// TxIndexKeyLength is the length of tx-index key
	TxIndexKeyLength = 1 + 8 + 8
	heightKeyLength  = 1 + 8
)

var _ TxIndexer = &KVIndexer{}

// KVIndexer implements a eth tx indexer on a KV db.
type KVIndexer struct {
	ctx           context.Context
	db            dbm.DB
	logger        *slog.Logger
	clientCtx     client.Context
	baseTraceTags gotracer.Tags
}

// NewKVIndexer creates the KVIndexer
func NewKVIndexer(db dbm.DB, logger *slog.Logger, clientCtx client.Context) *KVIndexer {
	return &KVIndexer{
		ctx:           nil,
		db:            db,
		logger:        logger,
		clientCtx:     clientCtx,
		baseTraceTags: newIndexerTraceTags(),
	}
}

// IndexBlock index all the eth txs in a block through the following steps:
// - Iterates over all of the Txs in Block
// - Parses eth Tx infos from cosmos-sdk events for every TxResult
// - Iterates over all the messages of the Tx
// - Builds and stores a indexer.TxResult based on parsed events for every message
func (kv *KVIndexer) IndexBlock(block *cmtypes.Block, txResults []*abci.ExecTxResult) (err error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	_, err = kv.IndexBlockWithStats(block, txResults)
	return err
}

func (kv *KVIndexer) IndexBlockWithStats(block *cmtypes.Block, txResults []*abci.ExecTxResult) (stats BlockIndexStats, err error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	defer func(err *error) {
		if e := recover(); e != nil {
			kv.logger.Debug("panic during parsing block results", "error", e)

			if ee, ok := e.(error); ok {
				*err = ee
			} else {
				*err = fmt.Errorf("panic during parsing block results: %v", e)
			}
		}
	}(&err)

	kv.logger.Debug("(KVIndexer) IndexBlock", "height", block.Height, "txns:", len(block.Txs))

	batch := kv.db.NewBatch()
	defer batch.Close()
	if err := kv.resetBlock(batch, block.Height); err != nil {
		return stats, err
	}

	blockHash := common.BytesToHash(block.Hash())
	flatLogs := make([]*ethtypes.Log, 0)
	blockLogs := make([][]*ethtypes.Log, 0)
	blockGasUsed := uint64(0)
	blockResultGasUsedBeforeTx := uint64(0)
	queryClient := rpctypes.NewQueryClient(kv.clientCtx)
	blockBaseFee := queryBlockBaseFee(kv.contextWithHeight(block.Height), queryClient, block.Height)

	// record index of valid eth tx during the iteration
	var ethTxIndex int32
	for txIndex, tx := range block.Txs {
		if txIndex >= len(txResults) {
			return stats, newBlockParseError(
				nil,
				"block %d txIndex %d: tx results shorter than block tx list (txCount=%d txResultCount=%d)",
				block.Height,
				txIndex,
				len(block.Txs),
				len(txResults),
			)
		}
		result := txResults[txIndex]
		if result == nil {
			return stats, newBlockParseError(nil, "block %d txIndex %d: missing tx result", block.Height, txIndex)
		}
		resultGasUsed := uint64(result.GasUsed)

		tx, err := kv.clientCtx.TxConfig.TxDecoder()(tx)
		if err != nil {
			return stats, newBlockParseError(err, "block %d txIndex %d: failed to decode tx", block.Height, txIndex)
		}

		if !isEthTx(tx) {
			blockGasUsed += resultGasUsed
			blockResultGasUsedBeforeTx += resultGasUsed
			continue
		}

		txs, err := rpctypes.ParseTxResult(result, tx)
		if err != nil {
			return stats, newBlockParseError(err, "block %d txIndex %d: failed to parse tx result", block.Height, txIndex)
		}

		var cumulativeTxEthGasUsed uint64
		for msgIndex, msg := range tx.GetMsgs() {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				// NOTE: non-evm msgs are ignored and excluded from cumulativeGasUsed.
				continue
			}

			var txHash common.Hash

			txResult := chaintypes.TxResult{
				Height:     block.Height,
				TxIndex:    uint32(txIndex),
				MsgIndex:   uint32(msgIndex),
				EthTxIndex: ethTxIndex,
			}
			if result.Code != abci.CodeTypeOK && result.Codespace != evmtypes.ModuleName {
				// exceeds block gas limit scenario, set gas used to gas limit because that's what's charged by ante handler.
				// some old versions don't emit any events, so workaround here directly.
				txResult.GasUsed = ethMsg.GetGas()
				txResult.Failed = true
				txHash = ethMsg.Hash()
			} else {
				// success or fail due to VM error

				parsedTx := txs.GetTxByMsgIndex(msgIndex)
				if parsedTx == nil {
					return stats, newBlockParseError(nil, "block %d txIndex %d msgIndex %d: msg index not found in results", block.Height, txIndex, msgIndex)
				}
				if parsedTx.EthTxIndex >= 0 && parsedTx.EthTxIndex != ethTxIndex {
					return stats, newBlockParseError(
						nil,
						"block %d txIndex %d msgIndex %d: eth tx index mismatch (expected=%d found=%d)",
						block.Height,
						txIndex,
						msgIndex,
						ethTxIndex,
						parsedTx.EthTxIndex,
					)
				}
				txResult.GasUsed = parsedTx.GasUsed
				txResult.Failed = parsedTx.Failed
				txHash = parsedTx.Hash
			}

			cumulativeTxEthGasUsed += txResult.GasUsed
			txResult.CumulativeGasUsed = cumulativeTxEthGasUsed

			if err := saveTxResult(kv.clientCtx.Codec, batch, txHash, &txResult); err != nil {
				return stats, errorsmod.Wrapf(err, "IndexBlock %d", block.Height)
			}

			txData := ethMsg.AsTransaction()
			if txData == nil {
				return stats, newBlockParseError(nil, "block %d txIndex %d msgIndex %d: failed to unpack eth tx data", block.Height, txIndex, msgIndex)
			}

			logs, err := evmtypes.DecodeMsgLogs(result.Data, msgIndex, uint64(block.Height))
			if err != nil {
				return stats, newBlockParseError(err, "block %d txIndex %d msgIndex %d: failed to decode msg logs", block.Height, txIndex, msgIndex)
			}

			status := uint64(ethtypes.ReceiptStatusSuccessful)
			if txResult.Failed {
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
				return stats, newBlockParseError(err, "block %d txIndex %d msgIndex %d txHash %s: failed to derive tx sender", block.Height, txIndex, msgIndex, txHash.Hex())
			}

			var contractAddress *common.Address
			if txData.To() == nil {
				addr := crypto.CreateAddress(from, txData.Nonce())
				contractAddress = &addr
			}

			receipt := buildCachedReceipt(
				status,
				blockResultGasUsedBeforeTx+cumulativeTxEthGasUsed,
				txResult.GasUsed,
				logs,
				txHash,
				contractAddress,
				blockHash,
				uint64(block.Height),
				uint64(ethTxIndex),
				txData.GasPrice(),
				from,
				txData.To(),
				uint64(txData.Type()),
			)
			if err := batch.Set(ReceiptKey(txHash), mustJSON(receipt)); err != nil {
				return stats, errorsmod.Wrapf(err, "IndexBlock %d, set receipt", block.Height)
			}

			rpcTx, err := rpctypes.NewRPCTransaction(
				ethMsg,
				blockHash,
				uint64(block.Height),
				uint64(ethTxIndex),
				blockBaseFee,
				txData.ChainId(),
			)
			if err != nil {
				return stats, newBlockParseError(err, "block %d txIndex %d msgIndex %d txHash %s: failed to build rpc tx", block.Height, txIndex, msgIndex, txHash.Hex())
			}
			if err := batch.Set(RPCtxHashKey(txHash), mustJSON(rpcTx)); err != nil {
				return stats, errorsmod.Wrapf(err, "IndexBlock %d, set rpc tx hash", block.Height)
			}
			if err := batch.Set(RPCtxIndexKey(block.Height, ethTxIndex), txHash.Bytes()); err != nil {
				return stats, errorsmod.Wrapf(err, "IndexBlock %d, set rpc tx index", block.Height)
			}

			blockLogs = append(blockLogs, logs)
			flatLogs = append(flatLogs, logs...)
			stats.IndexedEthTxs++
			ethTxIndex++
		}

		blockGasUsed += resultGasUsed
		blockResultGasUsedBeforeTx += resultGasUsed
	}

	blockBloom := evmtypes.LogsBloom(flatLogs)
	meta := CachedBlockMeta{
		Height:     block.Height,
		Hash:       blockHash.Hex(),
		ParentHash: common.BytesToHash(block.Header.LastBlockID.Hash).Hex(),
		Timestamp:  block.Time.Unix(),
		GasLimit:   0,
		GasUsed:    blockGasUsed,
		EthTxCount: ethTxIndex,
		TxCount:    int32(len(block.Txs)),
		Bloom:      hexutil.Encode(blockBloom),
	}

	if err := batch.Set(BlockMetaKey(block.Height), mustJSON(meta)); err != nil {
		return stats, errorsmod.Wrapf(err, "IndexBlock %d, set block meta", block.Height)
	}
	if err := batch.Set(BlockHashKey(blockHash), sdk.Uint64ToBigEndian(uint64(block.Height))); err != nil {
		return stats, errorsmod.Wrapf(err, "IndexBlock %d, set block hash map", block.Height)
	}
	if err := batch.Set(BlockLogsKey(block.Height), mustJSON(blockLogs)); err != nil {
		return stats, errorsmod.Wrapf(err, "IndexBlock %d, set block logs", block.Height)
	}

	if err := batch.Write(); err != nil {
		return stats, errorsmod.Wrapf(err, "IndexBlock %d, write batch", block.Height)
	}
	return stats, nil
}

// DeleteBlock removes all indexed data associated with a block height.
func (kv *KVIndexer) DeleteBlock(height int64) error {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	batch := kv.db.NewBatch()
	defer batch.Close()

	if err := kv.resetBlock(batch, height); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return errorsmod.Wrapf(err, "DeleteBlock %d", height)
	}

	return nil
}

// LastIndexedBlock returns the latest indexed block number, returns -1 if db is empty
func (kv *KVIndexer) LastIndexedBlock() (int64, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	return LoadLastBlock(kv.db)
}

// FirstIndexedBlock returns the first indexed block number, returns -1 if db is empty
func (kv *KVIndexer) FirstIndexedBlock() (int64, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	return LoadFirstBlock(kv.db)
}

// GetByTxHash finds eth tx by eth tx hash
func (kv *KVIndexer) GetByTxHash(hash common.Hash) (*chaintypes.TxResult, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(TxHashKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetByTxHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, fmt.Errorf("tx not found, hash: %s", hash.Hex())
	}
	var txKey chaintypes.TxResult
	if err := kv.clientCtx.Codec.Unmarshal(bz, &txKey); err != nil {
		return nil, errorsmod.Wrapf(err, "GetByTxHash %s", hash.Hex())
	}
	return &txKey, nil
}

// GetByBlockAndIndex finds eth tx by block number and eth tx index
func (kv *KVIndexer) GetByBlockAndIndex(blockNumber int64, txIndex int32) (*chaintypes.TxResult, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(TxIndexKey(blockNumber, txIndex))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetByBlockAndIndex %d %d", blockNumber, txIndex)
	}
	if len(bz) == 0 {
		return nil, fmt.Errorf("tx not found, block: %d, eth-index: %d", blockNumber, txIndex)
	}
	return kv.GetByTxHash(common.BytesToHash(bz))
}

// TxHashKey returns the key for db entry: `tx hash -> tx result struct`
func TxHashKey(hash common.Hash) []byte {
	return append([]byte{KeyPrefixTxHash}, hash.Bytes()...)
}

// TxIndexKey returns the key for db entry: `(block number, tx index) -> tx hash`
func TxIndexKey(blockNumber int64, txIndex int32) []byte {
	bz1 := sdk.Uint64ToBigEndian(uint64(blockNumber))
	bz2 := sdk.Uint64ToBigEndian(uint64(txIndex))
	return append(append([]byte{KeyPrefixTxIndex}, bz1...), bz2...)
}

// LoadLastBlock returns the latest indexed block number, returns -1 if db is empty
func LoadLastBlock(db dbm.DB) (int64, error) {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, txIndexerTraceTag)()

	itMeta, err := db.ReverseIterator([]byte{KeyPrefixBlockMeta}, []byte{KeyPrefixBlockMeta + 1})
	if err != nil {
		return 0, errorsmod.Wrap(err, "LoadLastBlock")
	}
	defer itMeta.Close()
	if itMeta.Valid() {
		return parseHeightFromHeightKey(itMeta.Key(), KeyPrefixBlockMeta)
	}

	it, err := db.ReverseIterator([]byte{KeyPrefixTxIndex}, []byte{KeyPrefixTxIndex + 1})
	if err != nil {
		return 0, errorsmod.Wrap(err, "LoadLastBlock")
	}
	defer it.Close()
	if !it.Valid() {
		return -1, nil
	}
	return parseBlockNumberFromKey(it.Key())
}

// LoadFirstBlock loads the first indexed block, returns -1 if db is empty
func LoadFirstBlock(db dbm.DB) (int64, error) {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, txIndexerTraceTag)()

	itMeta, err := db.Iterator([]byte{KeyPrefixBlockMeta}, []byte{KeyPrefixBlockMeta + 1})
	if err != nil {
		return 0, errorsmod.Wrap(err, "LoadFirstBlock")
	}
	defer itMeta.Close()
	if itMeta.Valid() {
		return parseHeightFromHeightKey(itMeta.Key(), KeyPrefixBlockMeta)
	}

	it, err := db.Iterator([]byte{KeyPrefixTxIndex}, []byte{KeyPrefixTxIndex + 1})
	if err != nil {
		return 0, errorsmod.Wrap(err, "LoadFirstBlock")
	}
	defer it.Close()
	if !it.Valid() {
		return -1, nil
	}
	return parseBlockNumberFromKey(it.Key())
}

// isEthTx check if the tx is an eth tx
func isEthTx(tx sdk.Tx) bool {
	extTx, ok := tx.(authante.HasExtensionOptionsTx)
	if !ok {
		return false
	}
	opts := extTx.GetExtensionOptions()
	if len(opts) != 1 || opts[0].GetTypeUrl() != "/injective.evm.v1.ExtensionOptionsEthereumTx" {
		return false
	}
	return true
}

// saveTxResult index the txResult into the kv db batch
func saveTxResult(codec sdkcodec.Codec, batch dbm.Batch, txHash common.Hash, txResult *chaintypes.TxResult) error {
	bz := codec.MustMarshal(txResult)
	if err := batch.Set(TxHashKey(txHash), bz); err != nil {
		return errorsmod.Wrap(err, "set tx-hash key")
	}
	if err := batch.Set(TxIndexKey(txResult.Height, txResult.EthTxIndex), txHash.Bytes()); err != nil {
		return errorsmod.Wrap(err, "set tx-index key")
	}
	return nil
}

func parseBlockNumberFromKey(key []byte) (int64, error) {
	if len(key) != TxIndexKeyLength {
		return 0, fmt.Errorf("wrong tx index key length, expect: %d, got: %d", TxIndexKeyLength, len(key))
	}

	return int64(sdk.BigEndianToUint64(key[1:9])), nil
}

func parseHeightFromHeightKey(key []byte, prefix byte) (int64, error) {
	if len(key) != heightKeyLength {
		return 0, fmt.Errorf("wrong height key length, expect: %d, got: %d", heightKeyLength, len(key))
	}
	if key[0] != prefix {
		return 0, fmt.Errorf("unexpected key prefix: %d", key[0])
	}
	return int64(sdk.BigEndianToUint64(key[1:])), nil
}

func queryBlockBaseFee(ctx context.Context, queryClient *rpctypes.QueryClient, height int64) *big.Int {
	defer gotracer.Trace(&ctx, txIndexerTraceTag)()

	res, err := queryClient.TxFeesQueryClient.GetEipBaseFee(rpctypes.ContextWithHeightFrom(ctx, height), &txfeestypes.QueryEipBaseFeeRequest{})
	if err != nil || res == nil || res.BaseFee == nil {
		return nil
	}
	return res.BaseFee.BaseFee.RoundInt().BigInt()
}

func (kv *KVIndexer) resetBlock(batch dbm.Batch, height int64) error {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	metaBz, err := kv.db.Get(BlockMetaKey(height))
	if err != nil {
		return errorsmod.Wrapf(err, "reset block %d: get meta", height)
	}
	if len(metaBz) > 0 {
		meta, err := unmarshalJSON[CachedBlockMeta](metaBz)
		if err != nil {
			return errorsmod.Wrapf(err, "reset block %d: unmarshal meta", height)
		}
		if err := batch.Delete(BlockHashKey(common.HexToHash(meta.Hash))); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete block hash", height)
		}
	}

	txIndexStart := txIndexPrefixStart(height)
	txIndexEnd := txIndexPrefixEnd(height)
	it, err := kv.db.Iterator(txIndexStart, txIndexEnd)
	if err != nil {
		return errorsmod.Wrapf(err, "reset block %d: tx iterator", height)
	}
	defer it.Close()

	for ; it.Valid(); it.Next() {
		txHash := common.BytesToHash(it.Value())
		if err := batch.Delete(it.Key()); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete tx index", height)
		}
		if err := batch.Delete(TxHashKey(txHash)); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete tx hash", height)
		}
		if err := batch.Delete(ReceiptKey(txHash)); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete receipt", height)
		}
		if err := batch.Delete(RPCtxHashKey(txHash)); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete rpc tx hash", height)
		}
	}

	rpcIndexStart := rpcTxIndexPrefixStart(height)
	rpcIndexEnd := rpcTxIndexPrefixEnd(height)
	rpcIt, err := kv.db.Iterator(rpcIndexStart, rpcIndexEnd)
	if err != nil {
		return errorsmod.Wrapf(err, "reset block %d: rpc tx iterator", height)
	}
	defer rpcIt.Close()

	for ; rpcIt.Valid(); rpcIt.Next() {
		if err := batch.Delete(rpcIt.Key()); err != nil {
			return errorsmod.Wrapf(err, "reset block %d: delete rpc tx index", height)
		}
	}

	if err := batch.Delete(BlockLogsKey(height)); err != nil {
		return errorsmod.Wrapf(err, "reset block %d: delete block logs", height)
	}
	if err := batch.Delete(BlockMetaKey(height)); err != nil {
		return errorsmod.Wrapf(err, "reset block %d: delete block meta", height)
	}

	return nil
}

func txIndexPrefixStart(height int64) []byte {
	return append([]byte{KeyPrefixTxIndex}, sdk.Uint64ToBigEndian(uint64(height))...)
}

func txIndexPrefixEnd(height int64) []byte {
	return append([]byte{KeyPrefixTxIndex}, sdk.Uint64ToBigEndian(uint64(height+1))...)
}

func rpcTxIndexPrefixStart(height int64) []byte {
	return append([]byte{KeyPrefixRPCtxIndex}, sdk.Uint64ToBigEndian(uint64(height))...)
}

func rpcTxIndexPrefixEnd(height int64) []byte {
	return append([]byte{KeyPrefixRPCtxIndex}, sdk.Uint64ToBigEndian(uint64(height+1))...)
}
