package indexer

import (
	"math/big"

	errorsmod "cosmossdk.io/errors"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func (kv *KVIndexer) saveIBCEVMHookRPCTransaction(
	batch dbm.Batch,
	height int64,
	txIndex int32,
	event *evmtypes.EventIBCEVMHookTx,
	blockHash common.Hash,
	cumulativeGasUsed uint64,
	logs []*virtualbank.RPCLog,
) error {
	response := event.GetResponse()
	txHash := common.HexToHash(response.Hash)
	status := uint64(ethtypes.ReceiptStatusSuccessful)
	if response.Failed() {
		status = uint64(ethtypes.ReceiptStatusFailed)
	}
	to := common.HexToAddress(event.Contract)
	receipt := buildCachedReceipt(
		status,
		cumulativeGasUsed,
		response.GasUsed,
		"",
		"",
		logs,
		txHash,
		nil,
		blockHash,
		uint64(height),
		uint64(txIndex),
		big.NewInt(0),
		common.HexToAddress(event.From),
		&to,
		uint64(ethtypes.LegacyTxType),
	)
	if err := batch.Set(ReceiptKey(txHash), mustMarshalReceipt(receipt)); err != nil {
		return errorsmod.Wrapf(err, "set IBC EVM hook receipt %s", txHash.Hex())
	}

	rpcTx := rpctypes.NewIBCEVMHookRPCTransaction(event, blockHash, uint64(height), uint64(txIndex), kv.virtualChainID)
	if err := batch.Set(RPCtxHashKey(txHash), mustMarshalRPCTransaction(rpcTx)); err != nil {
		return errorsmod.Wrapf(err, "set IBC EVM hook rpc tx hash %s", txHash.Hex())
	}
	if err := batch.Set(RPCtxIndexKey(height, txIndex), txHash.Bytes()); err != nil {
		return errorsmod.Wrapf(err, "set IBC EVM hook rpc tx index %d", txIndex)
	}
	return nil
}
