package debank

import (
	"encoding/json"
	"math/big"
	"sort"
	"testing"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

type fakeStateReader struct {
	proofs     map[common.Address]*rpctypes.AccountResult
	codes      map[common.Address]hexutil.Bytes
	proofCalls []common.Address
	codeCalls  []common.Address
}

func (f *fakeStateReader) GetProof(address common.Address, _ []string, block rpctypes.BlockNumberOrHash) (*rpctypes.AccountResult, error) {
	if block.BlockNumber == nil || *block.BlockNumber != 42 {
		panic("state proof queried at the wrong height")
	}
	f.proofCalls = append(f.proofCalls, address)
	return f.proofs[address], nil
}

func (f *fakeStateReader) GetCode(address common.Address, block rpctypes.BlockNumberOrHash) (hexutil.Bytes, error) {
	if block.BlockNumber == nil || *block.BlockNumber != 42 {
		panic("code queried at the wrong height")
	}
	f.codeCalls = append(f.codeCalls, address)
	return append(hexutil.Bytes(nil), f.codes[address]...), nil
}

func TestCalcValidationHashMatchesPipelineFixture(t *testing.T) {
	if got := CalcValidationHash([]string{"abcd", "efgh"}); got != 217265 {
		t.Fatalf("validation hash = %d, want 217265", got)
	}
}

func TestTraceConfigUsesOpcodeAndPrestateTracers(t *testing.T) {
	config := TraceConfig()
	if config.Tracer != "muxTracer" {
		t.Fatalf("tracer = %q, want muxTracer", config.Tracer)
	}
	var mux map[string]json.RawMessage
	if err := json.Unmarshal(config.TracerConfig, &mux); err != nil {
		t.Fatalf("invalid tracer config: %v", err)
	}
	if len(mux) != 2 || mux["erc7562Tracer"] == nil || mux["prestateTracer"] == nil {
		t.Fatalf("unexpected mux config: %s", config.TracerConfig)
	}
	var opcodeConfig struct {
		StackTopItemsSize int  `json:"stackTopItemsSize"`
		WithLog           bool `json:"withLog"`
	}
	if err := json.Unmarshal(mux["erc7562Tracer"], &opcodeConfig); err != nil {
		t.Fatalf("decode opcode tracer config: %v", err)
	}
	if !opcodeConfig.WithLog {
		t.Fatal("opcode tracer must collect logs")
	}
	if opcodeConfig.StackTopItemsSize != 3 {
		t.Fatalf("opcode tracer stack size = %d, want 3", opcodeConfig.StackTopItemsSize)
	}
}

func TestBuildEVMBlockWithNestedLogsStorageAndVirtualBankEvents(t *testing.T) {
	sender := common.HexToAddress("0x1000000000000000000000000000000000000001")
	token := common.HexToAddress("0x2000000000000000000000000000000000000002")
	callee := common.HexToAddress("0x3000000000000000000000000000000000000003")
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	blockHash := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	parentHash := common.HexToHash("0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	root := common.HexToHash("0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	parentRoot := common.HexToHash("0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	tokenSlot := common.HexToHash("0x01")
	calleeSlot := common.HexToHash("0x02")
	tokenCode := hexutil.Bytes{0x60, 0x00}
	calleeCode := hexutil.Bytes{0x60, 0x01}

	txIndex := hexutil.Uint64(0)
	block := rpcBlock{
		Number:           42,
		Hash:             blockHash,
		ParentHash:       parentHash,
		StateRoot:        root,
		Miner:            common.HexToAddress("0x4000000000000000000000000000000000000004"),
		Difficulty:       hexBig(0),
		GasLimit:         30_000_000,
		GasUsed:          90_000,
		Timestamp:        1_700_000_000,
		TransactionsRoot: common.HexToHash("0x11"),
		ReceiptsRoot:     common.HexToHash("0x12"),
		BaseFeePerGas:    hexBig(160_000_000),
		Transactions: []*rpctypes.RPCTransaction{{
			BlockHash:        &blockHash,
			BlockNumber:      hexBig(42),
			From:             sender,
			Gas:              120_000,
			GasPrice:         hexBig(170_000_000),
			GasFeeCap:        hexBig(200_000_000),
			GasTipCap:        hexBig(10_000_000),
			Hash:             txHash,
			Input:            hexutil.Bytes{0xa9, 0x05, 0x9c, 0xbb},
			Nonce:            7,
			To:               &token,
			TransactionIndex: &txIndex,
			Value:            hexBig(5),
			Type:             ethtypes.DynamicFeeTxType,
		}},
	}

	rootTopicBefore := common.HexToHash("0x101")
	childTopic := common.HexToHash("0x102")
	rootTopicAfter := common.HexToHash("0x103")
	rootFrame := &callFrame{
		Type:    "CALL",
		From:    sender,
		To:      &token,
		Gas:     120_000,
		GasUsed: 90_000,
		Input:   hexutil.Bytes{0xa9, 0x05, 0x9c, 0xbb},
		Value:   hexBig(5),
		Logs: []*callLog{
			{Address: token, Topics: []common.Hash{rootTopicBefore, common.HexToHash("0x201")}, Data: hexutil.Bytes{0x01}, Position: 0},
			{Address: token, Topics: []common.Hash{rootTopicAfter}, Data: hexutil.Bytes{0x03}, Position: 1},
		},
	}
	rootFrame.AccessedSlots.Writes = map[common.Hash]uint64{tokenSlot: 1}
	child := &callFrame{
		Type:    "DELEGATECALL",
		From:    callee,
		To:      &callee,
		Gas:     50_000,
		GasUsed: 30_000,
		Logs: []*callLog{{
			Address:  callee,
			Topics:   []common.Hash{childTopic},
			Data:     hexutil.Bytes{0x02},
			Position: 0,
		}},
	}
	child.AccessedSlots.Writes = map[common.Hash]uint64{calleeSlot: 1}
	rootFrame.Calls = []*callFrame{child}

	prestate := map[common.Address]*traceAccount{
		sender: {Balance: hexBig(1_000), Nonce: 7},
		token: {
			Balance: hexBig(0),
			Nonce:   1,
			Code:    tokenCode,
			Storage: map[common.Hash]common.Hash{tokenSlot: common.HexToHash("0x10")},
		},
		callee: {
			Balance: hexBig(0),
			Nonce:   1,
			Code:    calleeCode,
			Storage: map[common.Hash]common.Hash{calleeSlot: common.HexToHash("0x20")},
		},
	}
	mux := &muxTraceResult{CallTracer: rootFrame, PrestateTracer: prestate}

	virtualTopic := virtualbank.TopicTransfer
	receipt := rpcReceipt{
		Status:            hexutil.Uint64(ethtypes.ReceiptStatusSuccessful),
		GasUsed:           90_000,
		EffectiveGasPrice: hexBig(170_000_000),
		TransactionHash:   txHash,
		TransactionIndex:  0,
		Logs: []*virtualbank.RPCLog{
			{Address: token, Topics: rootFrame.Logs[0].Topics, Data: rootFrame.Logs[0].Data, Index: 10},
			{Address: callee, Topics: child.Logs[0].Topics, Data: child.Logs[0].Data, Index: 11},
			{Address: token, Topics: rootFrame.Logs[1].Topics, Data: rootFrame.Logs[1].Data, Index: 12},
			{Address: virtualbank.ContractAddress, Topics: []common.Hash{virtualTopic, common.HexToHash("0x301")}, Data: hexutil.Bytes{0x04}, Index: 13, Virtual: true},
		},
	}

	reader := &fakeStateReader{
		proofs: map[common.Address]*rpctypes.AccountResult{
			sender: proof(sender, 900, 8),
			token: proof(token, 0, 1, rpctypes.StorageResult{
				Key: tokenSlot.Hex(), Value: hexBig(0x11),
			}),
			callee: proof(callee, 0, 1, rpctypes.StorageResult{
				Key: calleeSlot.Hex(), Value: hexBig(0x21),
			}),
		},
		codes: map[common.Address]hexutil.Bytes{token: tokenCode, callee: calleeCode},
	}

	output, err := Build(BuildInput{
		Block:        asMap(t, block),
		ParentBlock:  asMap(t, rpcParentBlock{StateRoot: parentRoot}),
		Receipts:     []map[string]interface{}{asMap(t, receipt)},
		TraceResults: []*rpctypes.TxTraceResult{{TxHash: txHash, Result: asValue(t, mux)}},
		StateReader:  reader,
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if output.BlockFile.Block.ID != blockHash.Hex() || output.Header.Hash != blockHash {
		t.Fatalf("block/header hash mismatch: %#v %#v", output.BlockFile.Block.ID, output.Header.Hash)
	}
	if len(output.BlockFile.Txs) != 1 {
		t.Fatalf("tx count = %d, want 1", len(output.BlockFile.Txs))
	}
	gotTx := output.BlockFile.Txs[0]
	if gotTx.GasFeeCap.Cmp(big.NewInt(200_000_000)) != 0 || gotTx.GasTipCap.Cmp(big.NewInt(10_000_000)) != 0 {
		t.Fatalf("dynamic fee caps = %s/%s", gotTx.GasFeeCap, gotTx.GasTipCap)
	}

	rootID := pipelineID(txHash.Hex(), "", "0")
	childID := pipelineID(txHash.Hex(), rootID, "1")
	if len(output.BlockFile.Traces) != 2 {
		t.Fatalf("trace count = %d, want 2", len(output.BlockFile.Traces))
	}
	rootTrace, childTrace := output.BlockFile.Traces[0], output.BlockFile.Traces[1]
	if rootTrace.ID != rootID || !rootTrace.SelfStorageChange || !rootTrace.StorageChange || rootTrace.Subtraces != 1 {
		t.Fatalf("unexpected root trace: %#v", rootTrace)
	}
	if childTrace.ID != childID || childTrace.ParentTraceID != rootID || childTrace.PosInParentTrace != 1 {
		t.Fatalf("unexpected child trace identity: %#v", childTrace)
	}
	if childTrace.CallType != "delegatecall" || !childTrace.SelfStorageChange || len(childTrace.TraceAddress) != 1 || childTrace.TraceAddress[0] != 0 {
		t.Fatalf("unexpected child trace fields: %#v", childTrace)
	}

	if len(output.BlockFile.Events) != 4 {
		t.Fatalf("event count = %d, want 4", len(output.BlockFile.Events))
	}
	childEvent, beforeEvent, afterEvent, virtualEvent := output.BlockFile.Events[0], output.BlockFile.Events[1], output.BlockFile.Events[2], output.BlockFile.Events[3]
	assertEvent(t, childEvent, childID, 0, 11, childTopic)
	assertEvent(t, beforeEvent, rootID, 0, 10, rootTopicBefore)
	assertEvent(t, afterEvent, rootID, 2, 12, rootTopicAfter)
	assertEvent(t, virtualEvent, rootID, 3, 13, virtualTopic)
	if beforeEvent.Topics[0] != common.HexToHash("0x201").Hex() {
		t.Fatalf("event topics should exclude selector: %#v", beforeEvent.Topics)
	}

	wantStorageContracts := []string{lowerAddress(token), lowerAddress(callee)}
	sort.Strings(wantStorageContracts)
	if !equalStrings(output.BlockFile.StorageContracts, wantStorageContracts) {
		t.Fatalf("storage contracts = %#v, want %#v", output.BlockFile.StorageContracts, wantStorageContracts)
	}

	var stateDiff BlockStorageDiff
	if err := rlp.DecodeBytes(output.StateDiff, &stateDiff); err != nil {
		t.Fatalf("decode state diff: %v", err)
	}
	if stateDiff.Hash != root || stateDiff.ParentHash != parentRoot {
		t.Fatalf("state roots = %s/%s", stateDiff.Hash, stateDiff.ParentHash)
	}
	if len(stateDiff.NewAccounts) != 1 || stateDiff.NewAccounts[0].Address != crypto.Keccak256Hash(sender.Bytes()) || stateDiff.NewAccounts[0].Nonce != 8 {
		t.Fatalf("unexpected account diff: %#v", stateDiff.NewAccounts)
	}
	if len(stateDiff.StorageDiff) != 2 {
		t.Fatalf("storage diff count = %d, want 2", len(stateDiff.StorageDiff))
	}
	if output.ValidationHash != output.BlockFile.Validation().ValidationHash {
		t.Fatalf("validation hash = %d, want %d", output.ValidationHash, output.BlockFile.Validation().ValidationHash)
	}
	if len(reader.proofCalls) != 3 || len(reader.codeCalls) != 3 {
		t.Fatalf("post-state calls = %d proofs/%d code", len(reader.proofCalls), len(reader.codeCalls))
	}
}

func TestBuildPureCosmosVirtualTransaction(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01")
	blockHash := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb01")
	root := common.HexToHash("0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc01")
	parentRoot := common.HexToHash("0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd01")
	to := virtualbank.ContractAddress
	zero := hexBig(0)
	block := rpcBlock{
		Number:     42,
		Hash:       blockHash,
		ParentHash: common.HexToHash("0x01"),
		StateRoot:  root,
		Difficulty: zero,
		GasLimit:   30_000_000,
		Timestamp:  1_700_000_001,
		Transactions: []*rpctypes.RPCTransaction{{
			From:     common.Address{},
			To:       &to,
			Hash:     txHash,
			GasPrice: zero,
			Value:    zero,
			Type:     ethtypes.LegacyTxType,
			Virtual:  true,
		}},
	}
	receipt := rpcReceipt{
		Status:            hexutil.Uint64(ethtypes.ReceiptStatusSuccessful),
		EffectiveGasPrice: zero,
		TransactionHash:   txHash,
		Logs: []*virtualbank.RPCLog{{
			Address: virtualbank.ContractAddress,
			Topics:  []common.Hash{virtualbank.TopicTransfer},
			Index:   2,
			Virtual: true,
		}},
	}
	reader := &fakeStateReader{proofs: map[common.Address]*rpctypes.AccountResult{}, codes: map[common.Address]hexutil.Bytes{}}

	output, err := Build(BuildInput{
		Block:       asMap(t, block),
		ParentBlock: asMap(t, rpcParentBlock{StateRoot: parentRoot}),
		Receipts:    []map[string]interface{}{asMap(t, receipt)},
		StateReader: reader,
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if len(output.BlockFile.Txs) != 1 || len(output.BlockFile.Traces) != 1 || len(output.BlockFile.Events) != 1 {
		t.Fatalf("virtual artifacts = %d txs/%d traces/%d events", len(output.BlockFile.Txs), len(output.BlockFile.Traces), len(output.BlockFile.Events))
	}
	rootID := pipelineID(txHash.Hex(), "", "0")
	if output.BlockFile.Traces[0].ID != rootID || output.BlockFile.Traces[0].To != lowerAddress(virtualbank.ContractAddress) {
		t.Fatalf("unexpected virtual trace: %#v", output.BlockFile.Traces[0])
	}
	assertEvent(t, output.BlockFile.Events[0], rootID, 0, 2, virtualbank.TopicTransfer)
	if len(reader.proofCalls) != 0 || len(reader.codeCalls) != 0 {
		t.Fatal("pure Cosmos block must not query EVM accounts")
	}
	var stateDiff BlockStorageDiff
	if err := rlp.DecodeBytes(output.StateDiff, &stateDiff); err != nil {
		t.Fatalf("decode root-only state diff: %v", err)
	}
	if stateDiff.Hash != root || stateDiff.ParentHash != parentRoot || len(stateDiff.NewAccounts) != 0 {
		t.Fatalf("unexpected root-only state diff: %#v", stateDiff)
	}
}

func TestBuildRejectsMissingEVMTrace(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02")
	to := common.HexToAddress("0x2000000000000000000000000000000000000002")
	block := rpcBlock{
		Number:     42,
		Hash:       common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb02"),
		ParentHash: common.HexToHash("0x01"),
		StateRoot:  common.HexToHash("0x02"),
		Difficulty: hexBig(0),
		Transactions: []*rpctypes.RPCTransaction{{
			Hash: txHash, To: &to, GasPrice: hexBig(1), Value: hexBig(0),
		}},
	}
	reader := &fakeStateReader{proofs: map[common.Address]*rpctypes.AccountResult{}, codes: map[common.Address]hexutil.Bytes{}}
	_, err := Build(BuildInput{Block: asMap(t, block), StateReader: reader})
	if err == nil || err.Error() != "missing trace result for transaction "+txHash.Hex() {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildKeepsPersistedVirtualEventsSuccessfulWhenEVMReverts(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa03")
	to := common.HexToAddress("0x2000000000000000000000000000000000000002")
	block := rpcBlock{
		Number:     42,
		Hash:       common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"),
		ParentHash: common.HexToHash("0x01"),
		StateRoot:  common.HexToHash("0x02"),
		Difficulty: hexBig(0),
		Transactions: []*rpctypes.RPCTransaction{{
			Hash: txHash, To: &to, Gas: 100_000, GasPrice: hexBig(1), Value: hexBig(0),
		}},
	}
	frame := &callFrame{Type: "CALL", To: &to, Gas: 100_000, Error: "execution reverted"}
	frame.AccessedSlots.Writes = map[common.Hash]uint64{common.HexToHash("0x01"): 1}
	mux := &muxTraceResult{CallTracer: frame, PrestateTracer: map[common.Address]*traceAccount{}}
	receipt := rpcReceipt{
		Status:          hexutil.Uint64(ethtypes.ReceiptStatusFailed),
		GasUsed:         25_000,
		TransactionHash: txHash,
		Logs: []*virtualbank.RPCLog{{
			Address: virtualbank.ContractAddress,
			Topics:  []common.Hash{virtualbank.TopicCoinSpent},
			Index:   7,
			Virtual: true,
		}},
	}
	reader := &fakeStateReader{proofs: map[common.Address]*rpctypes.AccountResult{}, codes: map[common.Address]hexutil.Bytes{}}

	output, err := Build(BuildInput{
		Block:        asMap(t, block),
		Receipts:     []map[string]interface{}{asMap(t, receipt)},
		TraceResults: []*rpctypes.TxTraceResult{{TxHash: txHash, Result: asValue(t, mux)}},
		StateReader:  reader,
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if len(output.BlockFile.ErrorTraces) != 1 || len(output.BlockFile.Traces) != 0 {
		t.Fatalf("reverted trace classification = %d success/%d error", len(output.BlockFile.Traces), len(output.BlockFile.ErrorTraces))
	}
	if len(output.BlockFile.Events) != 1 || len(output.BlockFile.ErrorEvents) != 0 {
		t.Fatalf("virtual event classification = %d success/%d error", len(output.BlockFile.Events), len(output.BlockFile.ErrorEvents))
	}
	if output.BlockFile.Events[0].LogIndex != 7 {
		t.Fatalf("persisted virtual log index = %d, want 7", output.BlockFile.Events[0].LogIndex)
	}
	if want := []string{lowerAddress(to)}; !equalStrings(output.BlockFile.StorageContracts, want) {
		t.Fatalf("reverted SSTORE contracts = %#v, want %#v", output.BlockFile.StorageContracts, want)
	}
}

func TestFrameExecutionAddressUsesCallerStorageForDelegateVariants(t *testing.T) {
	caller := common.HexToAddress("0x1000000000000000000000000000000000000001")
	callee := common.HexToAddress("0x2000000000000000000000000000000000000002")
	for _, callType := range []string{"DELEGATECALL", "EXTDELEGATECALL", "CALLCODE"} {
		t.Run(callType, func(t *testing.T) {
			frame := &callFrame{Type: callType, From: caller, To: &callee}
			got, ok := frameExecutionAddress(frame)
			if !ok || got != caller {
				t.Fatalf("execution address = %s, want caller %s", got, caller)
			}
		})
	}
}

func TestAppendTransactionArtifactsSkipsRevertedLogsWhenAssigningReceiptIndexes(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa04")
	rootAddress := common.HexToAddress("0x1000000000000000000000000000000000000001")
	childAddress := common.HexToAddress("0x2000000000000000000000000000000000000002")
	revertedTopic := common.HexToHash("0x401")
	persistedTopic := common.HexToHash("0x402")

	root := &callFrame{
		Type: "CALL",
		To:   &rootAddress,
		Logs: []*callLog{{
			Address:  rootAddress,
			Topics:   []common.Hash{persistedTopic},
			Position: 1,
		}},
		Calls: []*callFrame{{
			Type:  "CALL",
			To:    &childAddress,
			Error: "execution reverted",
			Logs: []*callLog{{
				Address: childAddress,
				Topics:  []common.Hash{revertedTopic},
			}},
		}},
	}
	receipt := &rpcReceipt{
		Status: hexutil.Uint64(ethtypes.ReceiptStatusSuccessful),
		Logs: []*virtualbank.RPCLog{{
			Address: rootAddress,
			Topics:  []common.Hash{persistedTopic},
			Index:   17,
		}},
	}
	blockFile := &BlockFile{}
	appendTransactionArtifacts(blockFile, txHash, root, receipt, make(map[common.Address]struct{}))

	if len(blockFile.Events) != 1 || blockFile.Events[0].Selector != persistedTopic.Hex() || blockFile.Events[0].LogIndex != 17 {
		t.Fatalf("persisted events = %#v, want surviving log at index 17", blockFile.Events)
	}
	if len(blockFile.ErrorEvents) != 1 || blockFile.ErrorEvents[0].Selector != revertedTopic.Hex() || blockFile.ErrorEvents[0].LogIndex != 0 {
		t.Fatalf("error events = %#v, want reverted log at index zero", blockFile.ErrorEvents)
	}
}

func TestAppendTransactionArtifactsSkipsLogsWhoseParentReverted(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa05")
	rootAddress := common.HexToAddress("0x1000000000000000000000000000000000000001")
	childAddress := common.HexToAddress("0x2000000000000000000000000000000000000002")
	discardedTopic := common.HexToHash("0x501")

	root := &callFrame{
		Type:  "CALL",
		To:    &rootAddress,
		Error: "execution reverted",
		Calls: []*callFrame{{
			Type: "CALL",
			To:   &childAddress,
			Logs: []*callLog{{
				Address: childAddress,
				Topics:  []common.Hash{discardedTopic},
			}},
		}},
	}
	// A virtual event can survive a reverted EVM payload, but it must not be
	// consumed as the descendant's reverted native EVM log.
	receipt := &rpcReceipt{
		Status: hexutil.Uint64(ethtypes.ReceiptStatusFailed),
		Logs: []*virtualbank.RPCLog{{
			Address: virtualbank.ContractAddress,
			Topics:  []common.Hash{virtualbank.TopicTransfer},
			Index:   21,
			Virtual: true,
		}},
	}
	blockFile := &BlockFile{}
	appendTransactionArtifacts(blockFile, txHash, root, receipt, make(map[common.Address]struct{}))

	if len(blockFile.ErrorEvents) != 1 || blockFile.ErrorEvents[0].Selector != discardedTopic.Hex() || blockFile.ErrorEvents[0].LogIndex != 0 {
		t.Fatalf("error events = %#v, want parent-reverted log at index zero", blockFile.ErrorEvents)
	}
	if len(blockFile.Events) != 1 || blockFile.Events[0].Selector != virtualbank.TopicTransfer.Hex() || blockFile.Events[0].LogIndex != 21 {
		t.Fatalf("persisted virtual events = %#v", blockFile.Events)
	}
}

func TestAppendTransactionArtifactsClampsLogPositionWhenAssigningIndex(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa06")
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	topic := common.HexToHash("0x601")
	root := &callFrame{
		Type: "CALL",
		To:   &address,
		Logs: []*callLog{{Address: address, Topics: []common.Hash{topic}, Position: 99}},
	}
	receipt := &rpcReceipt{
		Status: hexutil.Uint64(ethtypes.ReceiptStatusSuccessful),
		Logs:   []*virtualbank.RPCLog{{Address: address, Topics: []common.Hash{topic}, Index: 23}},
	}
	blockFile := &BlockFile{}
	appendTransactionArtifacts(blockFile, txHash, root, receipt, make(map[common.Address]struct{}))

	if len(blockFile.Events) != 1 || blockFile.Events[0].LogIndex != 23 {
		t.Fatalf("events = %#v, want one clamped log at index 23", blockFile.Events)
	}
}

func TestBuildDoesNotRecordZeroStorageContractForFailedCreate(t *testing.T) {
	txHash := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa07")
	block := rpcBlock{
		Number:     42,
		Hash:       common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb07"),
		ParentHash: common.HexToHash("0x01"),
		StateRoot:  common.HexToHash("0x02"),
		Difficulty: hexBig(0),
		Transactions: []*rpctypes.RPCTransaction{{
			Hash: txHash, Gas: 200_000, GasPrice: hexBig(1), Value: hexBig(0),
		}},
	}
	frame := &callFrame{Type: "CREATE", Error: "execution reverted"}
	frame.AccessedSlots.Writes = map[common.Hash]uint64{common.HexToHash("0x01"): 1}
	receipt := rpcReceipt{
		Status:          hexutil.Uint64(ethtypes.ReceiptStatusFailed),
		GasUsed:         50_000,
		TransactionHash: txHash,
	}
	reader := &fakeStateReader{proofs: map[common.Address]*rpctypes.AccountResult{}, codes: map[common.Address]hexutil.Bytes{}}
	output, err := Build(BuildInput{
		Block:        asMap(t, block),
		Receipts:     []map[string]interface{}{asMap(t, receipt)},
		TraceResults: []*rpctypes.TxTraceResult{{TxHash: txHash, Result: asValue(t, &muxTraceResult{CallTracer: frame, PrestateTracer: map[common.Address]*traceAccount{}})}},
		StateReader:  reader,
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if len(output.BlockFile.StorageContracts) != 0 {
		t.Fatalf("failed constructor storage contracts = %#v, want none", output.BlockFile.StorageContracts)
	}
	if len(output.BlockFile.ErrorTraces) != 1 || !output.BlockFile.ErrorTraces[0].SelfStorageChange {
		t.Fatalf("failed constructor traces = %#v", output.BlockFile.ErrorTraces)
	}
	if _, ok := frameExecutionAddress(frame); ok {
		t.Fatal("failed CREATE without a destination must not resolve to the zero address")
	}
}

func assertEvent(t *testing.T, event Event, parentID string, position, logIndex int64, selector common.Hash) {
	t.Helper()
	if event.ID != pipelineID(parentID, new(big.Int).SetInt64(position).String()) || event.ParentTraceID != parentID || event.Position != position || event.LogIndex != logIndex || event.Selector != selector.Hex() {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func proof(address common.Address, balance int64, nonce uint64, storage ...rpctypes.StorageResult) *rpctypes.AccountResult {
	return &rpctypes.AccountResult{
		Address:      address,
		Balance:      hexBig(balance),
		Nonce:        hexutil.Uint64(nonce),
		StorageProof: storage,
	}
}

func hexBig(value int64) *hexutil.Big {
	return (*hexutil.Big)(big.NewInt(value))
}

func asMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return out
}

func asValue(t *testing.T, value interface{}) interface{} {
	t.Helper()
	var out interface{}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture value: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal fixture value: %v", err)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
