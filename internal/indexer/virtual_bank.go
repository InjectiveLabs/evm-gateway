package indexer

import (
	"math/big"

	errorsmod "cosmossdk.io/errors"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

func (kv *KVIndexer) buildVirtualCachedReceipt(
	status uint64,
	cumulativeGasUsed uint64,
	gasUsed uint64,
	logs []*virtualbank.RPCLog,
	txHash common.Hash,
	blockHash common.Hash,
	blockNumber uint64,
	transactionIndex uint64,
) CachedReceipt {
	to := virtualbank.ContractAddress
	return buildCachedReceipt(
		status,
		cumulativeGasUsed,
		gasUsed,
		"",
		"",
		logs,
		txHash,
		nil,
		blockHash,
		blockNumber,
		transactionIndex,
		big.NewInt(0),
		common.Address{},
		&to,
		uint64(ethtypes.LegacyTxType),
	)
}

func (kv *KVIndexer) saveVirtualRPCTransaction(
	batch dbm.Batch,
	height int64,
	txIndex int32,
	txHash common.Hash,
	blockHash common.Hash,
	receipt CachedReceipt,
	cosmosHash *common.Hash,
) error {
	if err := batch.Set(ReceiptKey(txHash), mustMarshalReceipt(receipt)); err != nil {
		return errorsmod.Wrapf(err, "set virtual receipt %s", txHash.Hex())
	}

	rpcTx := virtualbank.NewRPCTransaction(txHash, blockHash, uint64(height), uint64(txIndex), kv.virtualChainID, cosmosHash)
	if err := batch.Set(RPCtxHashKey(txHash), mustMarshalRPCTransaction(rpcTx)); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx hash %s", txHash.Hex())
	}
	if err := batch.Set(VirtualRPCtxKey(txHash), []byte{1}); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx marker %s", txHash.Hex())
	}
	if err := batch.Set(RPCtxIndexKey(height, txIndex), txHash.Bytes()); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx index %d", txIndex)
	}
	return nil
}
