package types

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

func TestTransactionArgsGettersAndString(t *testing.T) {
	from := common.HexToAddress("0x1000000000000000000000000000000000000001")
	data := hexutil.Bytes{0x1}
	input := hexutil.Bytes{0x2}
	args := &TransactionArgs{
		From:  &from,
		Data:  &data,
		Input: &input,
	}

	if got := args.GetFrom(); got != from {
		t.Fatalf("unexpected from: %s", got.Hex())
	}
	if got := args.GetData(); len(got) != 1 || got[0] != 0x2 {
		t.Fatalf("expected input to take precedence, got %x", got)
	}
	if !strings.Contains(args.String(), "TransactionArgs") {
		t.Fatalf("unexpected string output: %s", args.String())
	}

	args = &TransactionArgs{}
	if got := args.GetFrom(); got != (common.Address{}) {
		t.Fatalf("expected empty address, got %s", got.Hex())
	}
	if got := args.GetData(); got != nil {
		t.Fatalf("expected nil data, got %x", got)
	}
}

func TestTransactionArgsToTransaction(t *testing.T) {
	from := common.HexToAddress("0x2000000000000000000000000000000000000002")
	to := common.HexToAddress("0x3000000000000000000000000000000000000003")
	nonce := hexutil.Uint64(7)
	gas := hexutil.Uint64(21000)
	gasPrice := hexutil.Big(*big.NewInt(100))
	value := hexutil.Big(*big.NewInt(55))
	data := hexutil.Bytes{0xaa}
	chainID := hexutil.Big(*big.NewInt(1))
	accessList := ethtypes.AccessList{{Address: to}}

	legacy := (&TransactionArgs{
		From:     &from,
		To:       &to,
		Nonce:    &nonce,
		Gas:      &gas,
		GasPrice: &gasPrice,
		Value:    &value,
		Data:     &data,
	}).ToTransaction()
	if tx := legacy.AsTransaction(); tx.Type() != ethtypes.LegacyTxType || tx.Nonce() != 7 || tx.Gas() != 21000 || tx.To() == nil || *tx.To() != to {
		t.Fatalf("unexpected legacy tx: %+v", tx)
	}
	if got := common.BytesToAddress(legacy.From); got != from {
		t.Fatalf("unexpected legacy from: %s", got.Hex())
	}

	access := (&TransactionArgs{
		To:         &to,
		Nonce:      &nonce,
		Gas:        &gas,
		GasPrice:   &gasPrice,
		Value:      &value,
		Input:      &data,
		AccessList: &accessList,
		ChainID:    &chainID,
	}).ToTransaction()
	if tx := access.AsTransaction(); tx.Type() != ethtypes.AccessListTxType || len(tx.AccessList()) != 1 || tx.ChainId().Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("unexpected access-list tx: %+v", tx)
	}

	maxFee := hexutil.Big(*big.NewInt(300))
	maxTip := hexutil.Big(*big.NewInt(20))
	dynamic := (&TransactionArgs{
		To:                   &to,
		Nonce:                &nonce,
		Gas:                  &gas,
		MaxFeePerGas:         &maxFee,
		MaxPriorityFeePerGas: &maxTip,
		Value:                &value,
		Input:                &data,
		AccessList:           &accessList,
		ChainID:              &chainID,
	}).ToTransaction()
	if tx := dynamic.AsTransaction(); tx.Type() != ethtypes.DynamicFeeTxType || tx.GasFeeCap().Cmp(big.NewInt(300)) != 0 || tx.GasTipCap().Cmp(big.NewInt(20)) != 0 {
		t.Fatalf("unexpected dynamic-fee tx: %+v", tx)
	}
}

func TestTransactionArgsToMessage(t *testing.T) {
	gasPrice := hexutil.Big(*big.NewInt(5))
	maxFee := hexutil.Big(*big.NewInt(10))
	args := &TransactionArgs{
		GasPrice:     &gasPrice,
		MaxFeePerGas: &maxFee,
	}
	if _, err := args.ToMessage(0); err == nil {
		t.Fatal("expected conflicting fee args error")
	}

	from := common.HexToAddress("0x4000000000000000000000000000000000000004")
	to := common.HexToAddress("0x5000000000000000000000000000000000000005")
	gas := hexutil.Uint64(90000)
	value := hexutil.Big(*big.NewInt(77))
	nonce := hexutil.Uint64(8)
	input := hexutil.Bytes{0xbb, 0xcc}
	accessList := ethtypes.AccessList{{Address: to}}

	args = &TransactionArgs{
		From:       &from,
		To:         &to,
		Gas:        &gas,
		GasPrice:   &gasPrice,
		Value:      &value,
		Nonce:      &nonce,
		Input:      &input,
		AccessList: &accessList,
	}

	msg, err := args.ToMessage(50000)
	if err != nil {
		t.Fatalf("ToMessage returned error: %v", err)
	}
	if msg.From != from || msg.To == nil || *msg.To != to {
		t.Fatalf("unexpected message addresses: %+v", msg)
	}
	if msg.GasLimit != 50000 {
		t.Fatalf("expected gas to be capped at 50000, got %d", msg.GasLimit)
	}
	if msg.GasPrice.Cmp(big.NewInt(5)) != 0 || msg.GasFeeCap.Cmp(big.NewInt(5)) != 0 || msg.GasTipCap.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("unexpected gas pricing: gasPrice=%s feeCap=%s tipCap=%s", msg.GasPrice, msg.GasFeeCap, msg.GasTipCap)
	}
	if msg.Value.Cmp(big.NewInt(77)) != 0 || msg.Nonce != 8 {
		t.Fatalf("unexpected value/nonce: %+v", msg)
	}
	if len(msg.Data) != 2 || msg.Data[0] != 0xbb || len(msg.AccessList) != 1 {
		t.Fatalf("unexpected message data/access list: %+v", msg)
	}
	if !msg.SkipNonceChecks || !msg.SkipFromEOACheck {
		t.Fatalf("expected message skip flags to be set: %+v", msg)
	}

	msg, err = (&TransactionArgs{}).ToMessage(0)
	if err != nil {
		t.Fatalf("default ToMessage returned error: %v", err)
	}
	if msg.GasLimit != math.MaxUint64/2 {
		t.Fatalf("unexpected default gas limit: %d", msg.GasLimit)
	}
}
