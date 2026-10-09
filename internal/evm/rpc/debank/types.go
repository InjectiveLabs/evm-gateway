package debank

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

// Output is the response returned by trace_debankBlock. Its JSON and RLP
// layouts intentionally mirror github.com/Chaintable/pipeline/types.
type Output struct {
	BlockFile      *BlockFile    `json:"block_file"`
	Header         *Header       `json:"header"`
	StateDiff      hexutil.Bytes `json:"state_diff"`
	ValidationHash int64         `json:"validation_hash"`
}

type BlockFile struct {
	Block            Block         `json:"block"`
	Txs              []Transaction `json:"txs"`
	Events           []Event       `json:"events"`
	Traces           []Trace       `json:"traces"`
	ErrorEvents      []Event       `json:"error_events"`
	ErrorTraces      []Trace       `json:"error_traces"`
	StorageContracts []string      `json:"storage_contracts"`
}

type Block struct {
	ID                    string   `json:"id"`
	Height                *big.Int `json:"height"`
	ParentID              string   `json:"parent_id"`
	BaseFeePerGas         *big.Int `json:"base_fee_per_gas"`
	Miner                 string   `json:"miner"`
	GasLimit              *big.Int `json:"gas_limit"`
	GasUsed               *big.Int `json:"gas_used"`
	Timestamp             uint64   `json:"timestamp"`
	ProcessStartTimestamp int64    `json:"process_start_timestamp"`
}

type Transaction struct {
	ID               string        `json:"id"`
	From             string        `json:"from_addr"`
	To               string        `json:"to_addr"`
	Gas              *big.Int      `json:"gas_limit"`
	GasPrice         *big.Int      `json:"gas_price"`
	GasUsed          *big.Int      `json:"gas_used"`
	Status           bool          `json:"status"`
	GasFeeCap        *big.Int      `json:"max_fee_per_gas"`
	GasTipCap        *big.Int      `json:"max_priority_fee_per_gas"`
	Input            hexutil.Bytes `json:"input"`
	Nonce            *big.Int      `json:"nonce"`
	TransactionIndex int64         `json:"idx"`
	Value            *hexutil.Big  `json:"value"`
}

type Trace struct {
	ID                string        `json:"id"`
	From              string        `json:"from_addr"`
	Gas               *big.Int      `json:"gas_limit"`
	Input             hexutil.Bytes `json:"input"`
	To                string        `json:"to_addr"`
	Value             *hexutil.Big  `json:"value"`
	GasUsed           *big.Int      `json:"gas_used"`
	Output            hexutil.Bytes `json:"output"`
	CallCreateType    string        `json:"type"`
	CallType          string        `json:"call_type"`
	TxID              string        `json:"tx_id"`
	ParentTraceID     string        `json:"parent_trace_id"`
	PosInParentTrace  int64         `json:"pos_in_parent_trace"`
	SelfStorageChange bool          `json:"self_storage_change"`
	StorageChange     bool          `json:"storage_change"`
	Subtraces         int64         `json:"subtraces"`
	TraceAddress      []int64       `json:"trace_address"`
	Error             string        `json:"error,omitempty"`
}

type Event struct {
	ID            string        `json:"id"`
	Address       string        `json:"contract_id"`
	Selector      string        `json:"selector"`
	Topics        []string      `json:"topics"`
	Data          hexutil.Bytes `json:"data"`
	ParentTraceID string        `json:"parent_trace_id"`
	Position      int64         `json:"pos_in_parent_trace"`
	LogIndex      int64         `json:"idx"`
}

type Header struct {
	Number                *hexutil.Big        `json:"number"`
	Hash                  common.Hash         `json:"hash"`
	ParentHash            common.Hash         `json:"parentHash"`
	Nonce                 ethtypes.BlockNonce `json:"nonce"`
	MixHash               common.Hash         `json:"mixHash"`
	Sha3Uncles            common.Hash         `json:"sha3Uncles"`
	LogsBloom             ethtypes.Bloom      `json:"logsBloom"`
	StateRoot             common.Hash         `json:"stateRoot"`
	Miner                 common.Address      `json:"miner"`
	Difficulty            *hexutil.Big        `json:"difficulty"`
	ExtraData             hexutil.Bytes       `json:"extraData"`
	GasLimit              hexutil.Uint64      `json:"gasLimit"`
	GasUsed               hexutil.Uint64      `json:"gasUsed"`
	Timestamp             hexutil.Uint64      `json:"timestamp"`
	TransactionsRoot      common.Hash         `json:"transactionsRoot"`
	ReceiptsRoot          common.Hash         `json:"receiptsRoot"`
	BaseFeePerGas         *hexutil.Big        `json:"baseFeePerGas,omitempty"`
	WithdrawalsRoot       *common.Hash        `json:"withdrawalsRoot,omitempty"`
	BlobGasUsed           *hexutil.Uint64     `json:"blobGasUsed,omitempty"`
	ExcessBlobGas         *hexutil.Uint64     `json:"excessBlobGas,omitempty"`
	ParentBeaconBlockRoot *common.Hash        `json:"parentBeaconBlockRoot,omitempty"`
	RequestsRoot          *common.Hash        `json:"requestsRoot,omitempty"`
}

// The following types are RLP-encoded into Output.StateDiff. Field order and
// concrete integer types are part of the Pipeline wire contract.
type NewAccount struct {
	Address  common.Hash
	Balance  *uint256.Int
	Nonce    uint64
	CodeHash common.Hash
}

type NewCode struct {
	CodeHash common.Hash
	Code     []byte
}

type IndexValuePair struct {
	Index common.Hash
	Value *uint256.Int
}

type AccountStorageDiff struct {
	Address common.Hash
	Values  []IndexValuePair
}

type BlockStorageDiff struct {
	Hash            common.Hash
	ParentHash      common.Hash
	NewAccounts     []NewAccount
	DeletedAccounts []common.Hash
	StorageDiff     []AccountStorageDiff
	NewCodes        []NewCode
}

type BlockValidation struct {
	ValidationHash        int64 `json:"validation_hash"`
	IsFork                bool  `json:"is_fork"`
	TxsCount              int   `json:"txs_count"`
	EventsCount           int   `json:"events_count"`
	TracesCount           int   `json:"traces_count"`
	ErrorEventsCount      int   `json:"error_events_count"`
	ErrorTracesCount      int   `json:"error_traces_count"`
	StorageContractsCount int   `json:"storage_contracts_count"`
}

func (bf *BlockFile) Validation() BlockValidation {
	ids := make([]string, 0, 1+len(bf.Txs)+len(bf.Events)+len(bf.Traces))
	ids = append(ids, bf.Block.ID)
	for _, tx := range bf.Txs {
		ids = append(ids, tx.ID)
	}
	for _, event := range bf.Events {
		ids = append(ids, event.ID)
	}
	for _, trace := range bf.Traces {
		ids = append(ids, trace.ID)
	}

	return BlockValidation{
		ValidationHash:        CalcValidationHash(ids),
		TxsCount:              len(bf.Txs),
		EventsCount:           len(bf.Events),
		TracesCount:           len(bf.Traces),
		ErrorEventsCount:      len(bf.ErrorEvents),
		ErrorTracesCount:      len(bf.ErrorTraces),
		StorageContractsCount: len(bf.StorageContracts),
	}
}

// CalcValidationHash matches Pipeline's SHA-1 decimal-sum checksum.
func CalcValidationHash(ids []string) int64 {
	sum := new(big.Int)
	for _, id := range ids {
		digest := sha1.Sum([]byte(id))
		value := new(big.Int).SetBytes(digest[:])
		sum.Add(sum, value)
	}
	decimal := sum.String()
	if len(decimal) > 6 {
		decimal = decimal[len(decimal)-6:]
	}
	value, _ := strconv.ParseInt(decimal, 10, 64)
	return value
}

func pipelineID(parts ...string) string {
	// Keep this local rather than using fmt so the byte stream is exactly the
	// concatenation used by Pipeline's util.ToHash.
	h := md5.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func lowerAddress(address common.Address) string {
	return strings.ToLower(address.Hex())
}
