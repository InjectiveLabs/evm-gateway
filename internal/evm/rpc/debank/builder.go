package debank

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// StateReader supplies post-block account and storage state. Injective's gRPC
// prestate tracer exposes the values before execution but currently does not
// produce a usable post-state diff, so the gateway completes the diff with
// historical state queries at the traced height.
type StateReader interface {
	GetProof(address common.Address, storageKeys []string, blockNrOrHash rpctypes.BlockNumberOrHash) (*rpctypes.AccountResult, error)
	GetCode(address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error)
}

type BuildInput struct {
	Block        map[string]interface{}
	ParentBlock  map[string]interface{}
	Receipts     []map[string]interface{}
	TraceResults []*rpctypes.TxTraceResult
	StateReader  StateReader
}

type rpcBlock struct {
	Number           hexutil.Uint64             `json:"number"`
	Hash             common.Hash                `json:"hash"`
	ParentHash       common.Hash                `json:"parentHash"`
	Nonce            ethtypes.BlockNonce        `json:"nonce"`
	MixHash          common.Hash                `json:"mixHash"`
	Sha3Uncles       common.Hash                `json:"sha3Uncles"`
	LogsBloom        ethtypes.Bloom             `json:"logsBloom"`
	StateRoot        common.Hash                `json:"stateRoot"`
	Miner            common.Address             `json:"miner"`
	Difficulty       *hexutil.Big               `json:"difficulty"`
	ExtraData        hexutil.Bytes              `json:"extraData"`
	GasLimit         hexutil.Uint64             `json:"gasLimit"`
	GasUsed          hexutil.Uint64             `json:"gasUsed"`
	Timestamp        hexutil.Uint64             `json:"timestamp"`
	TransactionsRoot common.Hash                `json:"transactionsRoot"`
	ReceiptsRoot     common.Hash                `json:"receiptsRoot"`
	BaseFeePerGas    *hexutil.Big               `json:"baseFeePerGas,omitempty"`
	WithdrawalsRoot  *common.Hash               `json:"withdrawalsRoot,omitempty"`
	BlobGasUsed      *hexutil.Uint64            `json:"blobGasUsed,omitempty"`
	ExcessBlobGas    *hexutil.Uint64            `json:"excessBlobGas,omitempty"`
	ParentBeaconRoot *common.Hash               `json:"parentBeaconBlockRoot,omitempty"`
	RequestsRoot     *common.Hash               `json:"requestsRoot,omitempty"`
	Transactions     []*rpctypes.RPCTransaction `json:"transactions"`
}

type rpcParentBlock struct {
	StateRoot common.Hash `json:"stateRoot"`
}

type rpcReceipt struct {
	Status            hexutil.Uint64        `json:"status"`
	GasUsed           hexutil.Uint64        `json:"gasUsed"`
	EffectiveGasPrice *hexutil.Big          `json:"effectiveGasPrice"`
	TransactionHash   common.Hash           `json:"transactionHash"`
	TransactionIndex  hexutil.Uint64        `json:"transactionIndex"`
	ContractAddress   *common.Address       `json:"contractAddress"`
	Logs              []*virtualbank.RPCLog `json:"logs"`
}

type callLog struct {
	Address  common.Address `json:"address"`
	Topics   []common.Hash  `json:"topics"`
	Data     hexutil.Bytes  `json:"data"`
	Position hexutil.Uint   `json:"position"`

	pipelinePosition int64
	logIndex         int64
}

type callFrame struct {
	Type          string          `json:"type"`
	From          common.Address  `json:"from"`
	Gas           hexutil.Uint64  `json:"gas"`
	GasUsed       hexutil.Uint64  `json:"gasUsed"`
	To            *common.Address `json:"to,omitempty"`
	Input         hexutil.Bytes   `json:"input"`
	Output        hexutil.Bytes   `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
	RevertReason  string          `json:"revertReason,omitempty"`
	Calls         []*callFrame    `json:"calls,omitempty"`
	Logs          []*callLog      `json:"logs,omitempty"`
	Value         *hexutil.Big    `json:"value,omitempty"`
	AccessedSlots struct {
		Writes map[common.Hash]uint64 `json:"writes"`
	} `json:"accessedSlots"`

	traceID           string
	parentTraceID     string
	position          int64
	traceAddress      []int64
	parentFailed      bool
	selfStorageChange bool
	storageChange     bool
	timelineSize      int64
}

type traceAccount struct {
	Balance *hexutil.Big                `json:"balance,omitempty"`
	Code    hexutil.Bytes               `json:"code,omitempty"`
	Nonce   uint64                      `json:"nonce,omitempty"`
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
}

type muxTraceResult struct {
	CallTracer     *callFrame                       `json:"erc7562Tracer"`
	PrestateTracer map[common.Address]*traceAccount `json:"prestateTracer"`
}

type touchedAccount struct {
	pre     *traceAccount
	storage map[common.Hash]common.Hash
}

// BlockHeight decodes the concrete height from an eth_getBlock response.
func BlockHeight(raw map[string]interface{}) (rpctypes.BlockNumber, error) {
	var block struct {
		Number hexutil.Uint64 `json:"number"`
	}
	if err := remarshal(raw, &block); err != nil {
		return 0, fmt.Errorf("decode block height: %w", err)
	}
	if block.Number == 0 {
		return 0, fmt.Errorf("trace_debankBlock does not support a zero block height")
	}
	return rpctypes.BlockNumber(block.Number), nil
}

// TraceConfig returns the single-replay mux tracer configuration used by the
// Pipeline endpoint. erc7562Tracer supplies call frames, EVM logs, and exact
// per-frame SSTORE markers, while prestateTracer supplies every account and
// storage slot that must be compared with post-block state.
func TraceConfig() *rpctypes.TraceConfig {
	config := &rpctypes.TraceConfig{}
	config.Tracer = "muxTracer"
	// Keep the tracer's effective default explicit. Besides documenting the
	// required stack capture, this versions the trace-cache key so results made
	// before ordered mixed-block validation cannot be reused in offline mode.
	config.TracerConfig = json.RawMessage(`{"erc7562Tracer":{"stackTopItemsSize":3,"withLog":true},"prestateTracer":{}}`)
	return config
}

// Build converts the gateway's Ethereum block, receipts, mux traces, and
// virtual Cosmos logs into Pipeline's Mode 2 response contract.
func Build(input BuildInput) (*Output, error) {
	if input.StateReader == nil {
		return nil, fmt.Errorf("state reader is required")
	}

	var block rpcBlock
	if err := remarshal(input.Block, &block); err != nil {
		return nil, fmt.Errorf("decode block: %w", err)
	}
	if block.Number == 0 || block.Hash == (common.Hash{}) {
		return nil, fmt.Errorf("invalid traced block")
	}

	parentRoot := ethtypes.EmptyRootHash
	if input.ParentBlock != nil {
		var parent rpcParentBlock
		if err := remarshal(input.ParentBlock, &parent); err != nil {
			return nil, fmt.Errorf("decode parent block: %w", err)
		}
		if parent.StateRoot != (common.Hash{}) {
			parentRoot = parent.StateRoot
		}
	}

	receipts, err := decodeReceipts(input.Receipts)
	if err != nil {
		return nil, err
	}
	receiptsByHash := make(map[common.Hash]*rpcReceipt, len(receipts))
	for _, receipt := range receipts {
		if receipt == nil || receipt.TransactionHash == (common.Hash{}) {
			return nil, fmt.Errorf("receipt is missing transaction hash")
		}
		receiptsByHash[receipt.TransactionHash] = receipt
	}

	traceResults := make(map[common.Hash]*rpctypes.TxTraceResult, len(input.TraceResults))
	for _, result := range input.TraceResults {
		if result == nil || result.TxHash == (common.Hash{}) {
			continue
		}
		traceResults[result.TxHash] = result
	}

	muxByHash := make(map[common.Hash]*muxTraceResult, len(traceResults))
	touched := make(map[common.Address]*touchedAccount)
	for _, tx := range block.Transactions {
		if tx == nil {
			return nil, fmt.Errorf("block contains nil transaction")
		}
		if tx.Virtual {
			continue
		}
		result, ok := traceResults[tx.Hash]
		if !ok {
			return nil, fmt.Errorf("missing trace result for transaction %s", tx.Hash.Hex())
		}
		if result.Error != "" {
			return nil, fmt.Errorf("trace transaction %s: %s", tx.Hash.Hex(), result.Error)
		}
		var mux muxTraceResult
		if err := remarshal(result.Result, &mux); err != nil {
			return nil, fmt.Errorf("decode mux trace for transaction %s: %w", tx.Hash.Hex(), err)
		}
		if mux.CallTracer == nil {
			return nil, fmt.Errorf("call tracer result missing for transaction %s", tx.Hash.Hex())
		}
		muxByHash[tx.Hash] = &mux
		mergeTouchedAccounts(touched, mux.PrestateTracer)
	}

	blockNumber := rpctypes.BlockNumber(block.Number)
	blockRef := rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber}
	stateDiff, changedStorage, err := buildStateDiff(input.StateReader, blockRef, block.StateRoot, parentRoot, touched)
	if err != nil {
		return nil, err
	}

	blockFile := &BlockFile{
		Block:       buildBlock(&block),
		Txs:         make([]Transaction, 0, len(block.Transactions)),
		Events:      make([]Event, 0),
		Traces:      make([]Trace, 0),
		ErrorEvents: make([]Event, 0),
		ErrorTraces: make([]Trace, 0),
	}

	for _, tx := range block.Transactions {
		receipt, ok := receiptsByHash[tx.Hash]
		if !ok {
			return nil, fmt.Errorf("missing receipt for transaction %s", tx.Hash.Hex())
		}
		blockFile.Txs = append(blockFile.Txs, buildTransaction(tx, receipt))

		if tx.Virtual {
			root := syntheticVirtualFrame(tx, receipt)
			appendTransactionArtifacts(blockFile, tx.Hash, root, receipt, changedStorage)
			continue
		}
		appendTransactionArtifacts(blockFile, tx.Hash, muxByHash[tx.Hash].CallTracer, receipt, changedStorage)
	}
	// annotateFrame adds opcode-level SSTORE targets, including writes that are
	// reverted or leave the final value unchanged. Pipeline intentionally keeps
	// those contracts even when they do not appear in the net state diff.
	blockFile.StorageContracts = sortedAddresses(changedStorage)

	var stateDiffBytes []byte
	if stateDiff != nil {
		stateDiffBytes, err = rlp.EncodeToBytes(stateDiff)
		if err != nil {
			return nil, fmt.Errorf("encode state diff: %w", err)
		}
	}

	header := buildHeader(&block)
	validation := blockFile.Validation()
	return &Output{
		BlockFile:      blockFile,
		Header:         header,
		StateDiff:      hexutil.Bytes(stateDiffBytes),
		ValidationHash: validation.ValidationHash,
	}, nil
}

func remarshal(value interface{}, target interface{}) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func decodeReceipts(raw []map[string]interface{}) ([]*rpcReceipt, error) {
	receipts := make([]*rpcReceipt, len(raw))
	for i := range raw {
		var receipt rpcReceipt
		if err := remarshal(raw[i], &receipt); err != nil {
			return nil, fmt.Errorf("decode receipt %d: %w", i, err)
		}
		receipts[i] = &receipt
	}
	return receipts, nil
}

func buildBlock(block *rpcBlock) Block {
	baseFee := new(big.Int)
	if block.BaseFeePerGas != nil {
		baseFee.Set((*big.Int)(block.BaseFeePerGas))
	}
	return Block{
		ID:                    block.Hash.Hex(),
		Height:                new(big.Int).SetUint64(uint64(block.Number)),
		ParentID:              block.ParentHash.Hex(),
		BaseFeePerGas:         baseFee,
		Miner:                 lowerAddress(block.Miner),
		GasLimit:              new(big.Int).SetUint64(uint64(block.GasLimit)),
		GasUsed:               new(big.Int).SetUint64(uint64(block.GasUsed)),
		Timestamp:             uint64(block.Timestamp),
		ProcessStartTimestamp: time.Now().UnixMilli(),
	}
}

func buildHeader(block *rpcBlock) *Header {
	number := (*hexutil.Big)(new(big.Int).SetUint64(uint64(block.Number)))
	difficulty := block.Difficulty
	if difficulty == nil {
		difficulty = (*hexutil.Big)(new(big.Int))
	}
	return &Header{
		Number:                number,
		Hash:                  block.Hash,
		ParentHash:            block.ParentHash,
		Nonce:                 block.Nonce,
		MixHash:               block.MixHash,
		Sha3Uncles:            block.Sha3Uncles,
		LogsBloom:             block.LogsBloom,
		StateRoot:             block.StateRoot,
		Miner:                 block.Miner,
		Difficulty:            difficulty,
		ExtraData:             append(hexutil.Bytes(nil), block.ExtraData...),
		GasLimit:              block.GasLimit,
		GasUsed:               block.GasUsed,
		Timestamp:             block.Timestamp,
		TransactionsRoot:      block.TransactionsRoot,
		ReceiptsRoot:          block.ReceiptsRoot,
		BaseFeePerGas:         block.BaseFeePerGas,
		WithdrawalsRoot:       block.WithdrawalsRoot,
		BlobGasUsed:           block.BlobGasUsed,
		ExcessBlobGas:         block.ExcessBlobGas,
		ParentBeaconBlockRoot: block.ParentBeaconRoot,
		RequestsRoot:          block.RequestsRoot,
	}
}

func buildTransaction(tx *rpctypes.RPCTransaction, receipt *rpcReceipt) Transaction {
	to := common.Address{}
	if tx.To != nil {
		to = *tx.To
	} else if receipt.ContractAddress != nil {
		to = *receipt.ContractAddress
	}

	gasPrice := bigFromHex(tx.GasPrice)
	if receipt.EffectiveGasPrice != nil {
		gasPrice = bigFromHex(receipt.EffectiveGasPrice)
	}
	value := bigFromHex(tx.Value)
	gasFeeCap := new(big.Int)
	gasTipCap := new(big.Int)
	switch uint64(tx.Type) {
	case ethtypes.DynamicFeeTxType, ethtypes.BlobTxType, ethtypes.SetCodeTxType:
		gasFeeCap = bigFromHex(tx.GasFeeCap)
		gasTipCap = bigFromHex(tx.GasTipCap)
	}
	return Transaction{
		ID:               tx.Hash.Hex(),
		From:             lowerAddress(tx.From),
		To:               lowerAddress(to),
		Gas:              new(big.Int).SetUint64(uint64(tx.Gas)),
		GasPrice:         gasPrice,
		GasUsed:          new(big.Int).SetUint64(uint64(receipt.GasUsed)),
		Status:           uint64(receipt.Status) == ethtypes.ReceiptStatusSuccessful,
		GasFeeCap:        gasFeeCap,
		GasTipCap:        gasTipCap,
		Input:            append(hexutil.Bytes(nil), tx.Input...),
		Nonce:            new(big.Int).SetUint64(uint64(tx.Nonce)),
		TransactionIndex: int64(receipt.TransactionIndex),
		Value:            (*hexutil.Big)(value),
	}
}

func bigFromHex(value *hexutil.Big) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return new(big.Int).Set((*big.Int)(value))
}

func mergeTouchedAccounts(target map[common.Address]*touchedAccount, prestate map[common.Address]*traceAccount) {
	for address, account := range prestate {
		if account == nil {
			continue
		}
		touched, ok := target[address]
		if !ok {
			touched = &touchedAccount{
				pre: &traceAccount{
					Balance: cloneHexBig(account.Balance),
					Code:    append(hexutil.Bytes(nil), account.Code...),
					Nonce:   account.Nonce,
				},
				storage: make(map[common.Hash]common.Hash),
			}
			target[address] = touched
		}
		for slot, value := range account.Storage {
			if _, exists := touched.storage[slot]; !exists {
				touched.storage[slot] = value
			}
		}
	}
}

func cloneHexBig(value *hexutil.Big) *hexutil.Big {
	if value == nil {
		return nil
	}
	return (*hexutil.Big)(new(big.Int).Set((*big.Int)(value)))
}

func buildStateDiff(
	reader StateReader,
	blockRef rpctypes.BlockNumberOrHash,
	root common.Hash,
	parentRoot common.Hash,
	touched map[common.Address]*touchedAccount,
) (*BlockStorageDiff, map[common.Address]struct{}, error) {
	addresses := sortedTouchedAddresses(touched)
	diff := &BlockStorageDiff{Hash: root, ParentHash: parentRoot}
	changedStorage := make(map[common.Address]struct{})
	newCodes := make(map[common.Hash][]byte)

	for _, address := range addresses {
		entry := touched[address]
		slots := sortedSlots(entry.storage)
		storageKeys := make([]string, len(slots))
		for i, slot := range slots {
			storageKeys[i] = slot.Hex()
		}

		proof, err := reader.GetProof(address, storageKeys, blockRef)
		if err != nil {
			return nil, nil, fmt.Errorf("query post-state proof for %s: %w", address.Hex(), err)
		}
		if proof == nil {
			return nil, nil, fmt.Errorf("post-state proof for %s is nil", address.Hex())
		}
		code, err := reader.GetCode(address, blockRef)
		if err != nil {
			return nil, nil, fmt.Errorf("query post-state code for %s: %w", address.Hex(), err)
		}

		preBalance := bigFromHex(entry.pre.Balance)
		postBalance := bigFromHex(proof.Balance)
		preCodeHash := crypto.Keccak256Hash(entry.pre.Code)
		postCodeHash := crypto.Keccak256Hash(code)

		postStorage := make(map[common.Hash]common.Hash, len(proof.StorageProof))
		for i, storageProof := range proof.StorageProof {
			slot := common.HexToHash(storageProof.Key)
			if storageProof.Key == "" && i < len(slots) {
				slot = slots[i]
			}
			postStorage[slot] = common.BigToHash(bigFromHex(storageProof.Value))
		}

		preExists := accountExists(preBalance, entry.pre.Nonce, entry.pre.Code, entry.storage)
		postExists := accountExists(postBalance, uint64(proof.Nonce), code, postStorage)
		addressHash := crypto.Keccak256Hash(address.Bytes())
		if preExists && !postExists {
			diff.DeletedAccounts = append(diff.DeletedAccounts, addressHash)
			continue
		}

		basicChanged := preBalance.Cmp(postBalance) != 0 || entry.pre.Nonce != uint64(proof.Nonce) || preCodeHash != postCodeHash
		if basicChanged {
			balance, overflow := uint256.FromBig(postBalance)
			if overflow {
				return nil, nil, fmt.Errorf("post-state balance overflows uint256 for %s", address.Hex())
			}
			diff.NewAccounts = append(diff.NewAccounts, NewAccount{
				Address:  addressHash,
				Balance:  balance,
				Nonce:    uint64(proof.Nonce),
				CodeHash: postCodeHash,
			})
		}
		if preCodeHash != postCodeHash && len(code) > 0 {
			newCodes[postCodeHash] = append([]byte(nil), code...)
		}

		storageValues := make([]IndexValuePair, 0, len(slots))
		for _, slot := range slots {
			before := entry.storage[slot]
			after := postStorage[slot]
			if before == after {
				continue
			}
			value, overflow := uint256.FromBig(after.Big())
			if overflow {
				return nil, nil, fmt.Errorf("storage value overflows uint256 for %s slot %s", address.Hex(), slot.Hex())
			}
			storageValues = append(storageValues, IndexValuePair{
				Index: crypto.Keccak256Hash(slot.Bytes()),
				Value: value,
			})
		}
		if len(storageValues) > 0 {
			changedStorage[address] = struct{}{}
			diff.StorageDiff = append(diff.StorageDiff, AccountStorageDiff{
				Address: addressHash,
				Values:  storageValues,
			})
		}
	}

	codeHashes := make([]common.Hash, 0, len(newCodes))
	for codeHash := range newCodes {
		codeHashes = append(codeHashes, codeHash)
	}
	sort.Slice(codeHashes, func(i, j int) bool { return bytes.Compare(codeHashes[i][:], codeHashes[j][:]) < 0 })
	for _, codeHash := range codeHashes {
		diff.NewCodes = append(diff.NewCodes, NewCode{CodeHash: codeHash, Code: newCodes[codeHash]})
	}

	if root == parentRoot && len(diff.NewAccounts) == 0 && len(diff.DeletedAccounts) == 0 && len(diff.StorageDiff) == 0 && len(diff.NewCodes) == 0 {
		return nil, changedStorage, nil
	}
	return diff, changedStorage, nil
}

func accountExists(balance *big.Int, nonce uint64, code []byte, storage map[common.Hash]common.Hash) bool {
	if balance.Sign() != 0 || nonce != 0 || len(code) != 0 {
		return true
	}
	for _, value := range storage {
		if value != (common.Hash{}) {
			return true
		}
	}
	return false
}

func sortedTouchedAddresses(values map[common.Address]*touchedAccount) []common.Address {
	addresses := make([]common.Address, 0, len(values))
	for address := range values {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
	return addresses
}

func sortedSlots(values map[common.Hash]common.Hash) []common.Hash {
	slots := make([]common.Hash, 0, len(values))
	for slot := range values {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i][:], slots[j][:]) < 0 })
	return slots
}

func sortedAddresses(values map[common.Address]struct{}) []string {
	addresses := make([]common.Address, 0, len(values))
	for address := range values {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
	out := make([]string, len(addresses))
	for i, address := range addresses {
		out[i] = lowerAddress(address)
	}
	return out
}

func syntheticVirtualFrame(tx *rpctypes.RPCTransaction, receipt *rpcReceipt) *callFrame {
	to := tx.To
	value := tx.Value
	return &callFrame{
		Type:    "CALL",
		From:    tx.From,
		To:      to,
		Gas:     tx.Gas,
		GasUsed: receipt.GasUsed,
		Input:   append(hexutil.Bytes(nil), tx.Input...),
		Value:   value,
	}
}

func appendTransactionArtifacts(
	blockFile *BlockFile,
	txHash common.Hash,
	root *callFrame,
	receipt *rpcReceipt,
	changedStorage map[common.Address]struct{},
) {
	if uint64(receipt.Status) != ethtypes.ReceiptStatusSuccessful && root.Error == "" {
		root.Error = "execution reverted"
	}
	if root.GasUsed == 0 && receipt.GasUsed != 0 {
		root.GasUsed = receipt.GasUsed
	}

	root.traceID = pipelineID(txHash.Hex(), "", "0")
	root.position = 0
	root.traceAddress = []int64{}
	annotateFrame(root, txHash.Hex(), false, changedStorage)

	consumed := make(map[*virtualbank.RPCLog]bool)
	nativeLogs := make([]*virtualbank.RPCLog, 0, len(receipt.Logs))
	for _, log := range receipt.Logs {
		if log != nil && !log.Virtual {
			nativeLogs = append(nativeLogs, log)
		}
	}
	cursor := 0
	assignLogIndexes(root, nativeLogs, &cursor, consumed)

	rootTrace := frameToTrace(root, txHash.Hex())
	if root.failed() {
		blockFile.ErrorTraces = append(blockFile.ErrorTraces, rootTrace)
	} else {
		blockFile.Traces = append(blockFile.Traces, rootTrace)
	}
	appendNestedArtifacts(blockFile, root, txHash.Hex())

	position := root.timelineSize
	for _, log := range receipt.Logs {
		if log == nil || consumed[log] {
			continue
		}
		event := rpcLogToEvent(log, root.traceID, position)
		if root.failed() && !log.Virtual {
			event.LogIndex = 0
			blockFile.ErrorEvents = append(blockFile.ErrorEvents, event)
		} else {
			blockFile.Events = append(blockFile.Events, event)
		}
		position++
	}
}

func annotateFrame(frame *callFrame, txID string, parentFailed bool, changedStorage map[common.Address]struct{}) {
	frame.parentFailed = parentFailed
	assignTimelinePositions(frame, txID)

	frame.selfStorageChange = len(frame.AccessedSlots.Writes) > 0
	if frame.selfStorageChange {
		if executionAddress, ok := frameExecutionAddress(frame); ok {
			changedStorage[executionAddress] = struct{}{}
		}
	}
	frame.storageChange = frame.selfStorageChange
	for _, child := range frame.Calls {
		annotateFrame(child, txID, parentFailed || frame.failed(), changedStorage)
		if child.storageChange && !child.failed() {
			frame.storageChange = true
		}
	}
}

func assignTimelinePositions(frame *callFrame, txID string) {
	logsByCallCount := make(map[int][]*callLog)
	for _, log := range frame.Logs {
		if log == nil {
			continue
		}
		position := int(log.Position)
		if position < 0 || position > len(frame.Calls) {
			position = len(frame.Calls)
		}
		logsByCallCount[position] = append(logsByCallCount[position], log)
	}

	var next int64
	for callIndex := 0; callIndex <= len(frame.Calls); callIndex++ {
		for _, log := range logsByCallCount[callIndex] {
			log.pipelinePosition = next
			next++
		}
		if callIndex == len(frame.Calls) {
			continue
		}
		child := frame.Calls[callIndex]
		if child == nil {
			continue
		}
		child.parentTraceID = frame.traceID
		child.position = next
		child.traceID = pipelineID(txID, frame.traceID, strconv.FormatInt(next, 10))
		child.traceAddress = append(append([]int64(nil), frame.traceAddress...), int64(callIndex))
		next++
	}
	frame.timelineSize = next
}

func assignLogIndexes(
	frame *callFrame,
	nativeLogs []*virtualbank.RPCLog,
	cursor *int,
	consumed map[*virtualbank.RPCLog]bool,
) {
	logsByCallCount := make(map[int][]*callLog)
	for _, log := range frame.Logs {
		if log == nil {
			continue
		}
		position := int(log.Position)
		if position < 0 || position > len(frame.Calls) {
			position = len(frame.Calls)
		}
		logsByCallCount[position] = append(logsByCallCount[position], log)
	}
	frameLogsPersisted := !frame.failed() && !frame.parentFailed
	for callIndex := 0; callIndex <= len(frame.Calls); callIndex++ {
		if frameLogsPersisted {
			for _, log := range logsByCallCount[callIndex] {
				if *cursor >= len(nativeLogs) {
					return
				}
				rpcLog := nativeLogs[*cursor]
				log.logIndex = int64(rpcLog.Index)
				consumed[rpcLog] = true
				*cursor = *cursor + 1
			}
		}
		if callIndex < len(frame.Calls) && frame.Calls[callIndex] != nil {
			assignLogIndexes(frame.Calls[callIndex], nativeLogs, cursor, consumed)
		}
	}
}

func appendNestedArtifacts(blockFile *BlockFile, frame *callFrame, txID string) {
	for _, child := range frame.Calls {
		if child != nil {
			appendNestedArtifacts(blockFile, child, txID)
		}
	}
	for _, log := range frame.Logs {
		if log == nil {
			continue
		}
		event := callLogToEvent(log, frame.traceID)
		if frame.failed() || frame.parentFailed {
			event.LogIndex = 0
			blockFile.ErrorEvents = append(blockFile.ErrorEvents, event)
		} else {
			blockFile.Events = append(blockFile.Events, event)
		}
	}
	for _, child := range frame.Calls {
		if child == nil {
			continue
		}
		trace := frameToTrace(child, txID)
		if child.failed() {
			blockFile.ErrorTraces = append(blockFile.ErrorTraces, trace)
		} else {
			blockFile.Traces = append(blockFile.Traces, trace)
		}
	}
}

func frameExecutionAddress(frame *callFrame) (common.Address, bool) {
	switch strings.ToUpper(frame.Type) {
	case "DELEGATECALL", "EXTDELEGATECALL", "CALLCODE", "SELFDESTRUCT":
		return frame.From, true
	default:
		if frame.To != nil {
			return *frame.To, true
		}
		return common.Address{}, false
	}
}

func (frame *callFrame) failed() bool {
	return frame != nil && frame.Error != ""
}

func frameToTrace(frame *callFrame, txID string) Trace {
	to := common.Address{}
	if frame.To != nil {
		to = *frame.To
	}
	value := bigFromHex(frame.Value)
	callCreateType, callType := pipelineCallTypes(frame.Type)
	errorText := frame.Error
	if errorText != "" && frame.RevertReason != "" {
		errorText += ": " + frame.RevertReason
	}
	traceAddress := make([]int64, len(frame.traceAddress))
	copy(traceAddress, frame.traceAddress)
	return Trace{
		ID:                frame.traceID,
		From:              lowerAddress(frame.From),
		Gas:               new(big.Int).SetUint64(uint64(frame.Gas)),
		Input:             append(hexutil.Bytes(nil), frame.Input...),
		To:                lowerAddress(to),
		Value:             (*hexutil.Big)(value),
		GasUsed:           new(big.Int).SetUint64(uint64(frame.GasUsed)),
		Output:            append(hexutil.Bytes(nil), frame.Output...),
		CallCreateType:    callCreateType,
		CallType:          callType,
		TxID:              txID,
		ParentTraceID:     frame.parentTraceID,
		PosInParentTrace:  frame.position,
		SelfStorageChange: frame.selfStorageChange,
		StorageChange:     frame.storageChange,
		Subtraces:         int64(len(frame.Calls)),
		TraceAddress:      traceAddress,
		Error:             errorText,
	}
}

func pipelineCallTypes(raw string) (string, string) {
	switch strings.ToUpper(raw) {
	case "CREATE", "CREATE2":
		return "create", ""
	case "SELFDESTRUCT":
		return "suicide", ""
	case "CALL", "STATICCALL", "CALLCODE", "DELEGATECALL", "EXTDELEGATECALL":
		return "call", strings.ToLower(raw)
	default:
		return "empty", ""
	}
}

func callLogToEvent(log *callLog, parentTraceID string) Event {
	selector, topics := splitTopics(log.Topics)
	position := log.pipelinePosition
	return Event{
		ID:            pipelineID(parentTraceID, strconv.FormatInt(position, 10)),
		Address:       lowerAddress(log.Address),
		Selector:      selector,
		Topics:        topics,
		Data:          append(hexutil.Bytes(nil), log.Data...),
		ParentTraceID: parentTraceID,
		Position:      position,
		LogIndex:      log.logIndex,
	}
}

func rpcLogToEvent(log *virtualbank.RPCLog, parentTraceID string, position int64) Event {
	selector, topics := splitTopics(log.Topics)
	return Event{
		ID:            pipelineID(parentTraceID, strconv.FormatInt(position, 10)),
		Address:       lowerAddress(log.Address),
		Selector:      selector,
		Topics:        topics,
		Data:          append(hexutil.Bytes(nil), log.Data...),
		ParentTraceID: parentTraceID,
		Position:      position,
		LogIndex:      int64(log.Index),
	}
}

func splitTopics(values []common.Hash) (string, []string) {
	if len(values) == 0 {
		return "", []string{}
	}
	topics := make([]string, 0, len(values)-1)
	for _, topic := range values[1:] {
		topics = append(topics, topic.Hex())
	}
	return values[0].Hex(), topics
}
