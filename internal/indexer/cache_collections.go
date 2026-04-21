package indexer

import (
	"fmt"
	"math/big"

	errorsmod "cosmossdk.io/errors"
	"github.com/bytedance/sonic"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"upd.dev/xlab/gotracer"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

const (
	KeyPrefixRPCtxHash     = 3
	KeyPrefixRPCtxIndex    = 4
	KeyPrefixReceipt       = 5
	KeyPrefixBlockLogs     = 6
	KeyPrefixBlockMeta     = 7
	KeyPrefixBlockHash     = 8
	KeyPrefixVirtualRPCtx  = 11
	blockIndexedFlagLength = 1
)

type CachedBlockMeta struct {
	Height                  int64  `json:"height"`
	Hash                    string `json:"hash"`
	ParentHash              string `json:"parent_hash"`
	StateRoot               string `json:"state_root,omitempty"`
	Miner                   string `json:"miner,omitempty"`
	Timestamp               int64  `json:"timestamp"`
	Size                    uint64 `json:"size"`
	GasLimit                uint64 `json:"gas_limit"`
	GasUsed                 uint64 `json:"gas_used"`
	EthTxCount              int32  `json:"eth_tx_count"`
	TxCount                 int32  `json:"tx_count"`
	Bloom                   string `json:"bloom"`
	TransactionsRoot        string `json:"transactions_root,omitempty"`
	BaseFee                 string `json:"base_fee,omitempty"`
	VirtualizedCosmosEvents bool   `json:"virtualized_cosmos_events,omitempty"`
}

type CachedReceipt struct {
	Status            uint64                `json:"status"`
	CumulativeGasUsed uint64                `json:"cumulative_gas_used"`
	GasUsed           uint64                `json:"gas_used"`
	Reason            *string               `json:"reason,omitempty"`
	VMError           *string               `json:"vm_error,omitempty"`
	LogsBloom         string                `json:"logs_bloom"`
	Logs              []*virtualbank.RPCLog `json:"logs"`
	TransactionHash   string                `json:"transaction_hash"`
	ContractAddress   *string               `json:"contract_address,omitempty"`
	BlockHash         string                `json:"block_hash"`
	BlockNumber       uint64                `json:"block_number"`
	TransactionIndex  uint64                `json:"transaction_index"`
	EffectiveGasPrice string                `json:"effective_gas_price"`
	From              string                `json:"from"`
	To                *string               `json:"to,omitempty"`
	Type              uint64                `json:"type"`
}

func (r CachedReceipt) ToMap() map[string]interface{} {
	receipt := map[string]interface{}{
		"status":            hexutil.Uint(r.Status),
		"cumulativeGasUsed": hexutil.Uint64(r.CumulativeGasUsed),
		"logsBloom":         ethtypes.BytesToBloom(common.FromHex(r.LogsBloom)),
		"logs":              r.Logs,
		"transactionHash":   common.HexToHash(r.TransactionHash),
		"contractAddress":   nil,
		"gasUsed":           hexutil.Uint64(r.GasUsed),
		"blockHash":         r.BlockHash,
		"blockNumber":       hexutil.Uint64(r.BlockNumber),
		"transactionIndex":  hexutil.Uint64(r.TransactionIndex),
		"from":              common.HexToAddress(r.From),
		"to":                nil,
		"type":              hexutil.Uint(r.Type),
	}

	if r.Reason != nil {
		receipt["reason"] = *r.Reason
	}
	if r.VMError != nil {
		receipt["vmError"] = *r.VMError
	}
	if r.EffectiveGasPrice != "" {
		if p, err := hexutil.DecodeBig(r.EffectiveGasPrice); err == nil {
			receipt["effectiveGasPrice"] = (*hexutil.Big)(p)
		}
	}
	if r.ContractAddress != nil {
		receipt["contractAddress"] = common.HexToAddress(*r.ContractAddress)
	}
	if r.To != nil {
		to := common.HexToAddress(*r.To)
		receipt["to"] = &to
	}
	if r.Logs == nil {
		receipt["logs"] = []*virtualbank.RPCLog{}
	}

	return receipt
}

func RPCtxHashKey(hash common.Hash) []byte {
	return append([]byte{KeyPrefixRPCtxHash}, hash.Bytes()...)
}

func RPCtxIndexKey(blockNumber int64, txIndex int32) []byte {
	bz1 := sdk.Uint64ToBigEndian(uint64(blockNumber))
	bz2 := sdk.Uint64ToBigEndian(uint64(txIndex))
	return append(append([]byte{KeyPrefixRPCtxIndex}, bz1...), bz2...)
}

func ReceiptKey(hash common.Hash) []byte {
	return append([]byte{KeyPrefixReceipt}, hash.Bytes()...)
}

func VirtualRPCtxKey(hash common.Hash) []byte {
	return append([]byte{KeyPrefixVirtualRPCtx}, hash.Bytes()...)
}

func BlockLogsKey(height int64) []byte {
	return append([]byte{KeyPrefixBlockLogs}, sdk.Uint64ToBigEndian(uint64(height))...)
}

func BlockMetaKey(height int64) []byte {
	return append([]byte{KeyPrefixBlockMeta}, sdk.Uint64ToBigEndian(uint64(height))...)
}

func BlockHashKey(hash common.Hash) []byte {
	return append([]byte{KeyPrefixBlockHash}, hash.Bytes()...)
}

func mustJSON(v interface{}) []byte {
	bz, err := sonic.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bz
}

func unmarshalJSON[T any](bz []byte) (T, error) {
	var out T
	if len(bz) == 0 {
		return out, fmt.Errorf("empty payload")
	}
	if err := sonic.Unmarshal(bz, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (kv *KVIndexer) GetRPCTransactionByHash(hash common.Hash) (*rpctypes.RPCTransaction, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(RPCtxHashKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetRPCTransactionByHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("rpc tx not found, hash: %s", hash.Hex())
	}

	tx, err := unmarshalRPCTransactionPayload(bz)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetRPCTransactionByHash %s", hash.Hex())
	}
	return tx, nil
}

func (kv *KVIndexer) GetRPCTransactionByBlockAndIndex(blockNumber int64, txIndex int32) (*rpctypes.RPCTransaction, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	hashBz, err := kv.db.Get(RPCtxIndexKey(blockNumber, txIndex))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetRPCTransactionByBlockAndIndex %d %d", blockNumber, txIndex)
	}
	if len(hashBz) == 0 {
		return nil, newCacheMiss("rpc tx not found, block: %d, eth-index: %d", blockNumber, txIndex)
	}
	return kv.GetRPCTransactionByHash(common.BytesToHash(hashBz))
}

func (kv *KVIndexer) GetRPCTransactionHashesByBlockHeight(height int64) ([]common.Hash, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	it, err := kv.db.Iterator(rpcTxIndexPrefixStart(height), rpcTxIndexPrefixEnd(height))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetRPCTransactionHashesByBlockHeight %d", height)
	}
	defer it.Close()

	hashes := make([]common.Hash, 0)
	for ; it.Valid(); it.Next() {
		hashes = append(hashes, common.BytesToHash(it.Value()))
	}
	return hashes, nil
}

func (kv *KVIndexer) GetReceiptByTxHash(hash common.Hash) (map[string]interface{}, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(ReceiptKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetReceiptByTxHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("receipt not found, hash: %s", hash.Hex())
	}

	receipt, err := unmarshalReceiptPayload(bz)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetReceiptByTxHash %s", hash.Hex())
	}
	return receipt.ToMap(), nil
}

func (kv *KVIndexer) IsVirtualRPCTransaction(hash common.Hash) (bool, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(VirtualRPCtxKey(hash))
	if err != nil {
		return false, errorsmod.Wrapf(err, "IsVirtualRPCTransaction %s", hash.Hex())
	}
	return len(bz) > 0, nil
}

func (kv *KVIndexer) GetBlockMetaByHeight(height int64) (*CachedBlockMeta, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockMetaKey(height))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetBlockMetaByHeight %d", height)
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block meta not found, height: %d", height)
	}

	meta, err := unmarshalBlockMetaPayload(bz)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetBlockMetaByHeight %d", height)
	}
	return &meta, nil
}

func (kv *KVIndexer) GetBlockMetaByHash(hash common.Hash) (*CachedBlockMeta, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockHashKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetBlockMetaByHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block hash not indexed: %s", hash.Hex())
	}
	height := int64(sdk.BigEndianToUint64(bz))
	return kv.GetBlockMetaByHeight(height)
}

func (kv *KVIndexer) GetLogsByBlockHeight(height int64) ([][]*virtualbank.RPCLog, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockLogsKey(height))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetLogsByBlockHeight %d", height)
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block logs not found, height: %d", height)
	}
	return unmarshalBlockLogsPayload(bz)
}

func (kv *KVIndexer) GetFilteredLogsByBlockHeight(
	height int64,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]*virtualbank.RPCLog, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockLogsKey(height))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetFilteredLogsByBlockHeight %d", height)
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block logs not found, height: %d", height)
	}
	return unmarshalFilteredBlockLogsPayload(bz, addresses, topics)
}

func (kv *KVIndexer) GetLogsByBlockHash(hash common.Hash) ([][]*virtualbank.RPCLog, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockHashKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetLogsByBlockHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block hash not indexed: %s", hash.Hex())
	}
	height := int64(sdk.BigEndianToUint64(bz))
	return kv.GetLogsByBlockHeight(height)
}

func (kv *KVIndexer) GetFilteredLogsByBlockHash(
	hash common.Hash,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]*virtualbank.RPCLog, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockHashKey(hash))
	if err != nil {
		return nil, errorsmod.Wrapf(err, "GetFilteredLogsByBlockHash %s", hash.Hex())
	}
	if len(bz) == 0 {
		return nil, newCacheMiss("block hash not indexed: %s", hash.Hex())
	}
	height := int64(sdk.BigEndianToUint64(bz))
	return kv.GetFilteredLogsByBlockHeight(height, addresses, topics)
}

func (kv *KVIndexer) IsBlockIndexed(height int64) (bool, error) {
	ctx := kv.operationContext()
	if kv.ctx != nil {
		defer gotracer.Trace(&ctx, kv.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, kv.baseTraceTags)()
	}
	kv = kv.WithContext(ctx).(*KVIndexer)

	bz, err := kv.db.Get(BlockMetaKey(height))
	if err != nil {
		return false, errorsmod.Wrapf(err, "IsBlockIndexed %d", height)
	}
	return len(bz) == blockIndexedFlagLength || len(bz) > 0, nil
}

func buildCachedReceipt(
	status uint64,
	cumulativeGasUsed uint64,
	gasUsed uint64,
	reason string,
	vmError string,
	logs []*virtualbank.RPCLog,
	txHash common.Hash,
	contractAddress *common.Address,
	blockHash common.Hash,
	blockNumber uint64,
	transactionIndex uint64,
	effectiveGasPrice *big.Int,
	from common.Address,
	to *common.Address,
	txType uint64,
) CachedReceipt {
	var contractAddressStr *string
	if contractAddress != nil {
		v := contractAddress.Hex()
		contractAddressStr = &v
	}

	var toStr *string
	if to != nil {
		v := to.Hex()
		toStr = &v
	}

	var reasonStr *string
	if reason != "" {
		reasonStr = &reason
	}

	var vmErrorStr *string
	if vmError != "" {
		vmErrorStr = &vmError
	}

	return CachedReceipt{
		Status:            status,
		CumulativeGasUsed: cumulativeGasUsed,
		GasUsed:           gasUsed,
		Reason:            reasonStr,
		VMError:           vmErrorStr,
		LogsBloom:         hexutil.Encode(evmLogsBloom(logs)),
		Logs:              logs,
		TransactionHash:   txHash.Hex(),
		ContractAddress:   contractAddressStr,
		BlockHash:         blockHash.Hex(),
		BlockNumber:       blockNumber,
		TransactionIndex:  transactionIndex,
		EffectiveGasPrice: hexutil.EncodeBig(effectiveGasPrice),
		From:              from.Hex(),
		To:                toStr,
		Type:              txType,
	}
}

func evmLogsBloom(logs []*virtualbank.RPCLog) []byte {
	return evmtypes.LogsBloom(virtualbank.EthLogs(logs))
}
