package indexer

import (
	"fmt"
	"math/big"

	errorsmod "cosmossdk.io/errors"
	dbm "github.com/cosmos/cosmos-db"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
)

func cachedVirtualReceipt(receipt *virtual.Receipt) CachedReceipt {
	return buildCachedReceipt(
		receipt.Status,
		receipt.CumulativeGasUsed,
		receipt.GasUsed,
		receipt.Reason,
		receipt.VMError,
		receipt.Logs,
		receipt.TransactionHash,
		nil,
		receipt.BlockHash,
		receipt.BlockNumber,
		receipt.TransactionIndex,
		big.NewInt(0),
		receipt.From,
		receipt.To,
		receipt.Type,
	)
}

// saveVirtualTx stores a complete synthetic transaction and receipt.
func (kv *KVIndexer) saveVirtualTx(batch dbm.Batch, height int64, tx *virtual.Tx) error {
	if tx == nil || tx.Transaction == nil || tx.Receipt == nil {
		return fmt.Errorf("virtual transaction is incomplete")
	}

	txHash := tx.Transaction.Hash

	if err := batch.Set(ReceiptKey(txHash), mustMarshalReceipt(cachedVirtualReceipt(tx.Receipt))); err != nil {
		return errorsmod.Wrapf(err, "set virtual receipt %s", txHash.Hex())
	}

	if err := batch.Set(RPCtxHashKey(txHash), mustMarshalRPCTransaction(tx.Transaction)); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx hash %s", txHash.Hex())
	}

	if err := batch.Set(VirtualRPCtxKey(txHash), []byte{1}); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx marker %s", txHash.Hex())
	}

	if err := batch.Set(RPCtxIndexKey(height, int32(tx.Receipt.TransactionIndex)), txHash.Bytes()); err != nil {
		return errorsmod.Wrapf(err, "set virtual rpc tx index %d", tx.Receipt.TransactionIndex)
	}

	return nil
}
