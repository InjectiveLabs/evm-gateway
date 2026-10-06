package virtual

import (
	"encoding/binary"
	"math/big"

	"github.com/bytedance/sonic"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

const (
	blockPhaseBegin = "begin_block"
	blockPhaseEnd   = "end_block"
)

// LogContext carries the final RPC identity assigned to a group of logs.
type LogContext struct {
	BlockHash     common.Hash
	BlockNumber   uint64
	TxHash        common.Hash
	TxIndex       uint
	FirstLogIndex uint
	CosmosHash    *common.Hash
}

// RPCLog is the Ethereum log JSON shape plus metadata identifying synthesized
// Cosmos records.
type RPCLog struct {
	Address     common.Address `json:"address"`
	Topics      []common.Hash  `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	TxHash      common.Hash    `json:"transactionHash"`
	TxIndex     hexutil.Uint   `json:"transactionIndex"`
	BlockHash   common.Hash    `json:"blockHash"`
	Index       hexutil.Uint   `json:"logIndex"`
	Removed     bool           `json:"removed"`
	Virtual     bool           `json:"virtual,omitempty"`
	CosmosHash  *common.Hash   `json:"cosmos_hash,omitempty"`
}

type rpcLogJSON RPCLog

// Receipt is the complete Ethereum receipt representation for a synthesized
// Cosmos transaction.
type Receipt struct {
	Status            uint64
	CumulativeGasUsed uint64
	GasUsed           uint64
	Reason            string
	VMError           string
	Logs              []*RPCLog
	TransactionHash   common.Hash
	BlockHash         common.Hash
	BlockNumber       uint64
	TransactionIndex  uint64
	EffectiveGasPrice *big.Int
	From              common.Address
	To                *common.Address
	Type              uint64
}

// ToMap returns the receipt shape exposed by Ethereum JSON-RPC.
func (r *Receipt) ToMap() map[string]interface{} {
	logs := r.Logs
	if logs == nil {
		logs = []*RPCLog{}
	}

	price := r.EffectiveGasPrice
	if price == nil {
		price = big.NewInt(0)
	}

	out := map[string]interface{}{
		"status":            hexutil.Uint(r.Status),
		"cumulativeGasUsed": hexutil.Uint64(r.CumulativeGasUsed),
		"logsBloom":         ethtypes.BytesToBloom(LogsBloom(r.Logs)),
		"logs":              logs,
		"transactionHash":   r.TransactionHash,
		"contractAddress":   nil,
		"gasUsed":           hexutil.Uint64(r.GasUsed),
		"blockHash":         r.BlockHash.Hex(),
		"blockNumber":       hexutil.Uint64(r.BlockNumber),
		"transactionIndex":  hexutil.Uint64(r.TransactionIndex),
		"effectiveGasPrice": (*hexutil.Big)(new(big.Int).Set(price)),
		"from":              r.From,
		"to":                r.To,
		"type":              hexutil.Uint(r.Type),
	}
	if r.Reason != "" {
		out["reason"] = r.Reason
	}

	if r.VMError != "" {
		out["vmError"] = r.VMError
	}

	return out
}

// Tx is one fully materialized synthetic Ethereum transaction and receipt.
type Tx struct {
	Transaction *rpctypes.RPCTransaction
	Receipt     *Receipt
}

func LogMatches(log *RPCLog, addresses []common.Address, topics [][]common.Hash) bool {
	if log == nil {
		return false
	}

	if len(addresses) > 0 {
		matched := false
		for _, address := range addresses {
			if log.Address == address {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(topics) > len(log.Topics) {
		return false
	}

	for i, sub := range topics {
		matched := len(sub) == 0
		for _, topic := range sub {
			if log.Topics[i] == topic {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func (l *RPCLog) UnmarshalJSON(input []byte) error {
	var dec rpcLogJSON
	if err := sonic.Unmarshal(input, &dec); err != nil {
		return err
	}

	*l = RPCLog(dec)
	l.Topics = append([]common.Hash(nil), dec.Topics...)
	l.Data = append(hexutil.Bytes(nil), dec.Data...)
	l.CosmosHash = copyHashPtr(dec.CosmosHash)
	return nil
}

func NewRPCLog(log *ethtypes.Log, isVirtual bool, cosmosHash *common.Hash) *RPCLog {
	if log == nil {
		return nil
	}

	return &RPCLog{
		Address:     log.Address,
		Topics:      append([]common.Hash(nil), log.Topics...),
		Data:        append(hexutil.Bytes(nil), log.Data...),
		BlockNumber: hexutil.Uint64(log.BlockNumber),
		TxHash:      log.TxHash,
		TxIndex:     hexutil.Uint(log.TxIndex),
		BlockHash:   log.BlockHash,
		Index:       hexutil.Uint(log.Index),
		Removed:     log.Removed,
		Virtual:     isVirtual,
		CosmosHash:  copyHashPtr(cosmosHash),
	}
}

func WrapLogs(logs []*ethtypes.Log, isVirtual bool, cosmosHash *common.Hash) []*RPCLog {
	if logs == nil {
		return nil
	}

	out := make([]*RPCLog, 0, len(logs))
	for _, log := range logs {
		out = append(out, NewRPCLog(log, isVirtual, cosmosHash))
	}

	return out
}

func EthLog(log *RPCLog) *ethtypes.Log {
	if log == nil {
		return nil
	}

	return &ethtypes.Log{
		Address:     log.Address,
		Topics:      append([]common.Hash(nil), log.Topics...),
		Data:        append([]byte(nil), log.Data...),
		BlockNumber: uint64(log.BlockNumber),
		TxHash:      log.TxHash,
		TxIndex:     uint(log.TxIndex),
		BlockHash:   log.BlockHash,
		Index:       uint(log.Index),
		Removed:     log.Removed,
	}
}

func EthLogs(logs []*RPCLog) []*ethtypes.Log {
	if logs == nil {
		return nil
	}

	out := make([]*ethtypes.Log, 0, len(logs))
	for _, log := range logs {
		if converted := EthLog(log); converted != nil {
			out = append(out, converted)
		}
	}

	return out
}

func FlattenEthLogs(groups [][]*RPCLog) []*ethtypes.Log {
	out := make([]*ethtypes.Log, 0)
	for _, group := range groups {
		out = append(out, EthLogs(group)...)
	}

	return out
}

func LogsBloom(logs []*RPCLog) []byte {
	return evmtypes.LogsBloom(EthLogs(logs))
}

// SetLogMetadata assigns one contiguous segment of the block-wide log stream.
func SetLogMetadata(logs []*RPCLog, ctx LogContext) {
	for i, log := range logs {
		if log == nil {
			continue
		}
		log.BlockNumber = hexutil.Uint64(ctx.BlockNumber)
		log.TxHash = ctx.TxHash
		log.TxIndex = hexutil.Uint(ctx.TxIndex)
		log.BlockHash = ctx.BlockHash
		log.Index = hexutil.Uint(ctx.FirstLogIndex + uint(i))
		if log.Virtual {
			log.CosmosHash = copyHashPtr(ctx.CosmosHash)
		}
	}
}

func OriginalCosmosTxHash(tx cmtypes.Tx) common.Hash {
	return common.BytesToHash(tx.Hash())
}

func CosmosTxHash(tx cmtypes.Tx) common.Hash {
	return crypto.Keccak256Hash(OriginalCosmosTxHash(tx).Bytes())
}

func BeginBlockHash(height int64) common.Hash { return blockPhaseHash(blockPhaseBegin, height) }
func EndBlockHash(height int64) common.Hash   { return blockPhaseHash(blockPhaseEnd, height) }

func blockPhaseHash(phase string, height int64) common.Hash {
	payload := make([]byte, len(phase)+8)
	copy(payload, phase)
	binary.BigEndian.PutUint64(payload[len(phase):], uint64(height))

	return crypto.Keccak256Hash(payload)
}

func newRPCTransaction(
	hash, blockHash common.Hash,
	blockNumber, index uint64,
	chainID *big.Int,
	cosmosHash *common.Hash,
	from common.Address,
	to common.Address,
	input []byte,
	gasUsed uint64,
) *rpctypes.RPCTransaction {
	txIndex := hexutil.Uint64(index)
	zero := big.NewInt(0)

	var chainIDHex *hexutil.Big
	if chainID != nil {
		chainIDHex = (*hexutil.Big)(new(big.Int).Set(chainID))
	}

	return &rpctypes.RPCTransaction{
		BlockHash:        &blockHash,
		BlockNumber:      (*hexutil.Big)(new(big.Int).SetUint64(blockNumber)),
		From:             from,
		Gas:              hexutil.Uint64(gasUsed),
		GasPrice:         (*hexutil.Big)(new(big.Int).Set(zero)),
		Hash:             hash,
		Input:            append(hexutil.Bytes(nil), input...),
		Nonce:            0,
		To:               &to,
		TransactionIndex: &txIndex,
		Value:            (*hexutil.Big)(new(big.Int).Set(zero)),
		Type:             hexutil.Uint64(ethtypes.LegacyTxType),
		ChainID:          chainIDHex,
		V:                (*hexutil.Big)(new(big.Int).Set(zero)),
		R:                (*hexutil.Big)(new(big.Int).Set(zero)),
		S:                (*hexutil.Big)(new(big.Int).Set(zero)),
		Virtual:          true,
		CosmosHash:       copyHashPtr(cosmosHash),
	}
}

func copyHashPtr(hash *common.Hash) *common.Hash {
	if hash == nil {
		return nil
	}

	value := *hash
	return &value
}
