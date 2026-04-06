package types

import (
	"fmt"
	"math"
	"math/big"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
)

// TransactionArgs represents the arguments to construct a new transaction
// or a message call using JSON-RPC.
// Duplicate struct definition since geth struct is in an internal package.
type TransactionArgs struct {
	From                 *common.Address `json:"from"`
	To                   *common.Address `json:"to"`
	Gas                  *hexutil.Uint64 `json:"gas"`
	GasPrice             *hexutil.Big    `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big    `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big    `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big    `json:"value"`
	Nonce                *hexutil.Uint64 `json:"nonce"`

	// We accept "data" and "input" for backwards-compatibility reasons.
	// "input" is the newer name and should be preferred by clients.
	Data  *hexutil.Bytes `json:"data"`
	Input *hexutil.Bytes `json:"input"`

	// Introduced by AccessListTxType transaction.
	AccessList *ethtypes.AccessList `json:"accessList,omitempty"`
	ChainID    *hexutil.Big         `json:"chainId,omitempty"`
}

// String returns the struct in string format.
func (args *TransactionArgs) String() string {
	return fmt.Sprintf(
		"TransactionArgs{From:%v, To:%v, Gas:%v, Nonce:%v, Data:%v, Input:%v, AccessList:%v}",
		args.From,
		args.To,
		args.Gas,
		args.Nonce,
		args.Data,
		args.Input,
		args.AccessList,
	)
}

// ToTransaction converts the arguments to an ethereum transaction.
// This assumes SetTxDefaults has been called.
func (args *TransactionArgs) ToTransaction() *evmtypes.MsgEthereumTx {
	var gas, nonce uint64
	if args.Nonce != nil {
		nonce = uint64(*args.Nonce)
	}
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}

	var txData ethtypes.TxData
	switch {
	case args.MaxFeePerGas != nil:
		accessList := ethtypes.AccessList{}
		if args.AccessList != nil {
			accessList = *args.AccessList
		}
		txData = &ethtypes.DynamicFeeTx{
			To:         args.To,
			ChainID:    (*big.Int)(args.ChainID),
			Nonce:      nonce,
			Gas:        gas,
			GasFeeCap:  (*big.Int)(args.MaxFeePerGas),
			GasTipCap:  (*big.Int)(args.MaxPriorityFeePerGas),
			Value:      (*big.Int)(args.Value),
			Data:       args.GetData(),
			AccessList: accessList,
		}
	case args.AccessList != nil:
		txData = &ethtypes.AccessListTx{
			To:         args.To,
			ChainID:    (*big.Int)(args.ChainID),
			Nonce:      nonce,
			Gas:        gas,
			GasPrice:   (*big.Int)(args.GasPrice),
			Value:      (*big.Int)(args.Value),
			Data:       args.GetData(),
			AccessList: *args.AccessList,
		}
	default:
		txData = &ethtypes.LegacyTx{
			To:       args.To,
			Nonce:    nonce,
			Gas:      gas,
			GasPrice: (*big.Int)(args.GasPrice),
			Value:    (*big.Int)(args.Value),
			Data:     args.GetData(),
		}
	}

	tx := evmtypes.NewTxWithData(txData)
	if args.From != nil {
		tx.From = args.From.Bytes()
	}
	return tx
}

// ToMessage converts the arguments to the Message type used by the core EVM.
// This assumes SetTxDefaults has been called.
func (args *TransactionArgs) ToMessage(globalGasCap uint64) (*core.Message, error) {
	if args.GasPrice != nil && (args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil) {
		return nil, errors.New("both gasPrice and (maxFeePerGas or maxPriorityFeePerGas) specified")
	}

	addr := args.GetFrom()

	gas := globalGasCap
	if gas == 0 {
		gas = uint64(math.MaxUint64 / 2)
	}
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	if globalGasCap != 0 && globalGasCap < gas {
		gas = globalGasCap
	}

	gasPrice := new(big.Int)
	if args.GasPrice != nil {
		gasPrice = args.GasPrice.ToInt()
	}
	gasFeeCap := gasPrice
	gasTipCap := gasPrice

	value := new(big.Int)
	if args.Value != nil {
		value = args.Value.ToInt()
	}
	data := args.GetData()

	var accessList ethtypes.AccessList
	if args.AccessList != nil {
		accessList = *args.AccessList
	}

	var nonce uint64
	if args.Nonce != nil {
		nonce = uint64(*args.Nonce)
	}

	msg := &core.Message{
		From:             addr,
		To:               args.To,
		Nonce:            nonce,
		Value:            value,
		GasLimit:         gas,
		GasPrice:         gasPrice,
		GasFeeCap:        gasFeeCap,
		GasTipCap:        gasTipCap,
		Data:             data,
		AccessList:       accessList,
		SkipNonceChecks:  true,
		SkipFromEOACheck: true,
	}

	return msg, nil
}

// GetFrom retrieves the transaction sender address.
func (args *TransactionArgs) GetFrom() common.Address {
	if args.From == nil {
		return common.Address{}
	}
	return *args.From
}

// GetData retrieves the transaction calldata. Input is preferred over Data.
func (args *TransactionArgs) GetData() []byte {
	if args.Input != nil {
		return *args.Input
	}
	if args.Data != nil {
		return *args.Data
	}
	return nil
}
