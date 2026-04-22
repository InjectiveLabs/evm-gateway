package indexer

import (
	"math/big"
	"testing"

	txsigning "cosmossdk.io/x/tx/signing"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/merkle"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	gogoproto "github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	protov2 "google.golang.org/protobuf/proto"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

func TestKVIndexerDeleteBlockRemovesIndexedDataForHeight(t *testing.T) {
	db := dbm.NewMemDB()
	kv := &KVIndexer{db: db, logger: testLogger()}

	height := int64(42)
	otherHeight := int64(43)
	blockHash := common.HexToHash("0x42")
	otherBlockHash := common.HexToHash("0x43")
	txHashA := common.HexToHash("0xaa")
	txHashB := common.HexToHash("0xbb")
	txHashOther := common.HexToHash("0xcc")

	mustSet := func(key, value []byte) {
		t.Helper()
		if err := db.Set(key, value); err != nil {
			t.Fatalf("set %x: %v", key, err)
		}
	}
	assertMissing := func(key []byte) {
		t.Helper()
		bz, err := db.Get(key)
		if err != nil {
			t.Fatalf("get %x: %v", key, err)
		}
		if len(bz) != 0 {
			t.Fatalf("expected key %x to be deleted, got %x", key, bz)
		}
	}
	assertPresent := func(key []byte) {
		t.Helper()
		bz, err := db.Get(key)
		if err != nil {
			t.Fatalf("get %x: %v", key, err)
		}
		if len(bz) == 0 {
			t.Fatalf("expected key %x to remain present", key)
		}
	}

	mustSet(BlockMetaKey(height), mustJSON(CachedBlockMeta{Height: height, Hash: blockHash.Hex()}))
	mustSet(BlockHashKey(blockHash), sdk.Uint64ToBigEndian(uint64(height)))
	mustSet(BlockLogsKey(height), mustJSON([][]string{{"stale"}}))
	mustSet(TxIndexKey(height, 0), txHashA.Bytes())
	mustSet(TxIndexKey(height, 1), txHashB.Bytes())
	mustSet(TxHashKey(txHashA), []byte("tx-a"))
	mustSet(TxHashKey(txHashB), []byte("tx-b"))
	mustSet(ReceiptKey(txHashA), []byte("{}"))
	mustSet(ReceiptKey(txHashB), []byte("{}"))
	mustSet(RPCtxHashKey(txHashA), []byte("{}"))
	mustSet(RPCtxHashKey(txHashB), []byte("{}"))
	mustSet(VirtualRPCtxKey(txHashB), []byte{1})
	mustSet(RPCtxIndexKey(height, 0), txHashA.Bytes())
	mustSet(RPCtxIndexKey(height, 1), txHashB.Bytes())
	mustSet(TraceTxKey(txHashA, nil), []byte(`{"type":"call"}`))
	mustSet(TraceTxKey(txHashB, nil), []byte(`{"type":"call"}`))
	mustSet(TraceBlockKey(height, nil), []byte(`[{"result":{"type":"call"}}]`))

	mustSet(BlockMetaKey(otherHeight), mustJSON(CachedBlockMeta{Height: otherHeight, Hash: otherBlockHash.Hex()}))
	mustSet(BlockHashKey(otherBlockHash), sdk.Uint64ToBigEndian(uint64(otherHeight)))
	mustSet(TxIndexKey(otherHeight, 0), txHashOther.Bytes())
	mustSet(TxHashKey(txHashOther), []byte("tx-other"))
	mustSet(TraceTxKey(txHashOther, nil), []byte(`{"type":"call"}`))
	mustSet(TraceBlockKey(otherHeight, nil), []byte(`[{"result":{"type":"call"}}]`))

	if err := kv.DeleteBlock(height); err != nil {
		t.Fatalf("DeleteBlock returned error: %v", err)
	}

	assertMissing(BlockMetaKey(height))
	assertMissing(BlockHashKey(blockHash))
	assertMissing(BlockLogsKey(height))
	assertMissing(TxIndexKey(height, 0))
	assertMissing(TxIndexKey(height, 1))
	assertMissing(TxHashKey(txHashA))
	assertMissing(TxHashKey(txHashB))
	assertMissing(ReceiptKey(txHashA))
	assertMissing(ReceiptKey(txHashB))
	assertMissing(RPCtxHashKey(txHashA))
	assertMissing(RPCtxHashKey(txHashB))
	assertMissing(VirtualRPCtxKey(txHashB))
	assertMissing(RPCtxIndexKey(height, 0))
	assertMissing(RPCtxIndexKey(height, 1))
	assertMissing(TraceTxKey(txHashA, nil))
	assertMissing(TraceTxKey(txHashB, nil))
	assertMissing(TraceBlockKey(height, nil))

	assertPresent(BlockMetaKey(otherHeight))
	assertPresent(BlockHashKey(otherBlockHash))
	assertPresent(TxIndexKey(otherHeight, 0))
	assertPresent(TxHashKey(txHashOther))
	assertPresent(TraceTxKey(txHashOther, nil))
	assertPresent(TraceBlockKey(otherHeight, nil))
}

func TestKVIndexerCachedBlockLookupsUseHashAndRPCIndexCollections(t *testing.T) {
	db := dbm.NewMemDB()
	kv := &KVIndexer{db: db, logger: testLogger()}

	height := int64(42)
	otherHeight := int64(43)
	blockHash := common.HexToHash("0x42")
	otherBlockHash := common.HexToHash("0x43")
	txHashA := common.HexToHash("0xaa")
	txHashB := common.HexToHash("0xbb")
	txHashOther := common.HexToHash("0xcc")

	mustSet := func(key, value []byte) {
		t.Helper()
		if err := db.Set(key, value); err != nil {
			t.Fatalf("set %x: %v", key, err)
		}
	}

	mustSet(BlockMetaKey(height), mustJSON(CachedBlockMeta{Height: height, Hash: blockHash.Hex(), GasLimit: 12345}))
	mustSet(BlockHashKey(blockHash), sdk.Uint64ToBigEndian(uint64(height)))
	mustSet(RPCtxIndexKey(height, 1), txHashB.Bytes())
	mustSet(RPCtxIndexKey(height, 0), txHashA.Bytes())

	mustSet(BlockMetaKey(otherHeight), mustJSON(CachedBlockMeta{Height: otherHeight, Hash: otherBlockHash.Hex()}))
	mustSet(BlockHashKey(otherBlockHash), sdk.Uint64ToBigEndian(uint64(otherHeight)))
	mustSet(RPCtxIndexKey(otherHeight, 0), txHashOther.Bytes())

	meta, err := kv.GetBlockMetaByHash(blockHash)
	if err != nil {
		t.Fatalf("GetBlockMetaByHash returned error: %v", err)
	}
	if meta.Height != height {
		t.Fatalf("unexpected meta height: got %d want %d", meta.Height, height)
	}
	if meta.Hash != blockHash.Hex() {
		t.Fatalf("unexpected meta hash: got %s want %s", meta.Hash, blockHash.Hex())
	}

	hashes, err := kv.GetRPCTransactionHashesByBlockHeight(height)
	if err != nil {
		t.Fatalf("GetRPCTransactionHashesByBlockHeight returned error: %v", err)
	}
	if len(hashes) != 2 {
		t.Fatalf("unexpected tx hash count: got %d want 2", len(hashes))
	}
	if hashes[0] != txHashA || hashes[1] != txHashB {
		t.Fatalf("unexpected tx hashes order: got %v want [%s %s]", hashes, txHashA.Hex(), txHashB.Hex())
	}
}

// TestKVIndexerIndexesVirtualBankTransfersForCosmosAndFinalizeEvents verifies
// indexed virtual bank logs, receipts, and RPC transactions for Cosmos and
// begin/end block events.
func TestKVIndexerIndexesVirtualBankTransfersForCosmosAndFinalizeEvents(t *testing.T) {
	db := dbm.NewMemDB()
	tx := testSDKTx{msgs: []sdk.Msg{&banktypes.MsgSend{}}}
	kv := NewKVIndexer(
		db,
		testLogger(),
		client.Context{TxConfig: testTxConfig{tx: tx}},
		WithVirtualBankTransfers(true, "1337"),
	)

	block := tmtypes.MakeBlock(7, []tmtypes.Tx{tmtypes.Tx("cosmos-bank-tx")}, nil, nil)
	blockResults := &coretypes.ResultBlockResults{
		Height: 7,
		TxResults: []*abci.ExecTxResult{
			{
				Code:    abci.CodeTypeOK,
				GasUsed: 42,
				Events: []abci.Event{
					{
						Type: virtualbank.EventTypeTransfer,
						Attributes: []abci.EventAttribute{
							{Key: "sender", Value: "0x1111111111111111111111111111111111111111"},
							{Key: "recipient", Value: "0x2222222222222222222222222222222222222222"},
							{Key: "amount", Value: "100inj"},
							{Key: virtualbank.AttributeMsgIndex, Value: "0"},
						},
					},
				},
			},
		},
		FinalizeBlockEvents: []abci.Event{
			{
				Type: virtualbank.EventTypeCoinReceived,
				Attributes: []abci.EventAttribute{
					{Key: "receiver", Value: "0x4444444444444444444444444444444444444444"},
					{Key: "amount", Value: "6inj"},
					{Key: virtualbank.AttributeMode, Value: virtualbank.ModeBeginBlock},
				},
			},
			{
				Type: virtualbank.EventTypeBurn,
				Attributes: []abci.EventAttribute{
					{Key: "burner", Value: "0x3333333333333333333333333333333333333333"},
					{Key: "amount", Value: "5inj"},
					{Key: virtualbank.AttributeMode, Value: virtualbank.ModeEndBlock},
				},
			},
		},
	}

	if err := kv.IndexBlockWithResults(block, blockResults); err != nil {
		t.Fatalf("IndexBlockWithResults returned error: %v", err)
	}

	meta, err := kv.GetBlockMetaByHeight(7)
	if err != nil {
		t.Fatalf("GetBlockMetaByHeight returned error: %v", err)
	}
	if !meta.VirtualizedCosmosEvents {
		t.Fatal("expected block meta to record virtualized cosmos event mode")
	}
	if meta.EthTxCount != 3 {
		t.Fatalf("unexpected visible tx count: got %d want 3", meta.EthTxCount)
	}

	hashes, err := kv.GetRPCTransactionHashesByBlockHeight(7)
	if err != nil {
		t.Fatalf("GetRPCTransactionHashesByBlockHeight returned error: %v", err)
	}
	if len(hashes) != 3 {
		t.Fatalf("unexpected hash count: got %d want 3", len(hashes))
	}
	if hashes[0] != virtualbank.BeginBlockHash(7) {
		t.Fatalf("unexpected begin block virtual hash: got %s want %s", hashes[0], virtualbank.BeginBlockHash(7))
	}
	if hashes[1] != virtualbank.CosmosTxHash(block.Txs[0]) {
		t.Fatalf("unexpected cosmos virtual hash: got %s want %s", hashes[1], virtualbank.CosmosTxHash(block.Txs[0]))
	}
	if hashes[2] != virtualbank.EndBlockHash(7) {
		t.Fatalf("unexpected finalize virtual hash: got %s want %s", hashes[2], virtualbank.EndBlockHash(7))
	}
	expectedRoot := common.BytesToHash(merkle.HashFromByteSlices([][]byte{
		virtualbank.BeginBlockHash(7).Bytes(),
		virtualbank.CosmosTxHash(block.Txs[0]).Bytes(),
		virtualbank.EndBlockHash(7).Bytes(),
	})).Hex()
	if meta.TransactionsRoot != expectedRoot {
		t.Fatalf("unexpected transactions root: got %s want %s", meta.TransactionsRoot, expectedRoot)
	}
	if meta.TransactionsRoot == common.BytesToHash(block.Header.DataHash).Hex() {
		t.Fatalf("transactions root still uses raw comet data hash: %s", meta.TransactionsRoot)
	}

	rpcTx, err := kv.GetRPCTransactionByBlockAndIndex(7, 1)
	if err != nil {
		t.Fatalf("GetRPCTransactionByBlockAndIndex returned error: %v", err)
	}
	if rpcTx.Hash != hashes[1] {
		t.Fatalf("unexpected rpc tx hash: got %s want %s", rpcTx.Hash, hashes[1])
	}
	if rpcTx.To == nil || *rpcTx.To != virtualbank.ContractAddress {
		t.Fatalf("expected virtual tx to target reserved contract address, got %v", rpcTx.To)
	}
	if len(rpcTx.Input) != 0 {
		t.Fatalf("expected virtual tx input to be empty, got %s", hexutil.Encode(rpcTx.Input))
	}
	if !rpcTx.Virtual {
		t.Fatal("expected virtual tx field to be true")
	}
	originalCosmosHash := virtualbank.OriginalCosmosTxHash(block.Txs[0])
	if rpcTx.CosmosHash == nil || *rpcTx.CosmosHash != originalCosmosHash {
		t.Fatalf("unexpected cosmos hash: got %v want %s", rpcTx.CosmosHash, originalCosmosHash.Hex())
	}
	isVirtual, err := kv.IsVirtualRPCTransaction(hashes[1])
	if err != nil {
		t.Fatalf("IsVirtualRPCTransaction returned error: %v", err)
	}
	if !isVirtual {
		t.Fatal("expected cosmos tx marker to be virtual")
	}

	logs, err := kv.GetLogsByBlockHeight(7)
	if err != nil {
		t.Fatalf("GetLogsByBlockHeight returned error: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("unexpected log groups: got %d want 3", len(logs))
	}
	if logs[0][0].Topics[0] != virtualbank.TopicCoinReceived || logs[0][0].TxIndex != 0 || logs[0][0].Index != 0 {
		t.Fatalf("unexpected first virtual log: topic=%s tx=%d index=%d", logs[0][0].Topics[0], logs[0][0].TxIndex, logs[0][0].Index)
	}
	if !logs[0][0].Virtual || logs[0][0].CosmosHash != nil {
		t.Fatalf("unexpected begin block virtual metadata: virtual=%t cosmos_hash=%v", logs[0][0].Virtual, logs[0][0].CosmosHash)
	}
	if logs[1][0].Topics[0] != virtualbank.TopicTransfer || logs[1][0].TxIndex != 1 || logs[1][0].Index != 1 {
		t.Fatalf("unexpected cosmos virtual log: topic=%s tx=%d index=%d", logs[1][0].Topics[0], logs[1][0].TxIndex, logs[1][0].Index)
	}
	if !logs[1][0].Virtual || logs[1][0].CosmosHash == nil || *logs[1][0].CosmosHash != originalCosmosHash {
		t.Fatalf("unexpected cosmos virtual metadata: virtual=%t cosmos_hash=%v", logs[1][0].Virtual, logs[1][0].CosmosHash)
	}
	if logs[2][0].Topics[0] != virtualbank.TopicBurn || logs[2][0].TxIndex != 2 || logs[2][0].Index != 2 {
		t.Fatalf("unexpected finalize virtual log: topic=%s tx=%d index=%d", logs[2][0].Topics[0], logs[2][0].TxIndex, logs[2][0].Index)
	}
	if !logs[2][0].Virtual || logs[2][0].CosmosHash != nil {
		t.Fatalf("unexpected end block virtual metadata: virtual=%t cosmos_hash=%v", logs[2][0].Virtual, logs[2][0].CosmosHash)
	}

	receipt, err := kv.GetReceiptByTxHash(hashes[1])
	if err != nil {
		t.Fatalf("GetReceiptByTxHash returned error: %v", err)
	}
	receiptLogs, ok := receipt["logs"].([]*virtualbank.RPCLog)
	if !ok || len(receiptLogs) != 1 {
		t.Fatalf("unexpected receipt logs: %#v", receipt["logs"])
	}
	if !receiptLogs[0].Virtual || receiptLogs[0].CosmosHash == nil || *receiptLogs[0].CosmosHash != originalCosmosHash {
		t.Fatalf("unexpected receipt virtual metadata: virtual=%t cosmos_hash=%v", receiptLogs[0].Virtual, receiptLogs[0].CosmosHash)
	}
}

func TestKVIndexerKeepsEthTxIndexEVMOrdinalWhenVirtualTransfersShiftRPCIndex(t *testing.T) {
	db := dbm.NewMemDB()
	chainID := big.NewInt(1337)
	signer := ethtypes.LatestSignerForChainID(chainID)
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe51296170827965fb7b05b8a9e7f6f2")
	if err != nil {
		t.Fatalf("HexToECDSA: %v", err)
	}
	to := common.HexToAddress("0x1000000000000000000000000000000000000001")
	ethTx := ethtypes.NewTx(&ethtypes.LegacyTx{
		Nonce:    1,
		To:       &to,
		Value:    big.NewInt(0),
		Gas:      21000,
		GasPrice: big.NewInt(1),
	})
	signedTx, err := ethtypes.SignTx(ethTx, signer, key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	ethMsg := &evmtypes.MsgEthereumTx{}
	if err := ethMsg.FromSignedEthereumTx(signedTx, signer); err != nil {
		t.Fatalf("FromSignedEthereumTx: %v", err)
	}

	decodedTx := testSDKTx{
		msgs:             []sdk.Msg{ethMsg},
		extensionOptions: evmExtensionOptions(),
	}
	kv := NewKVIndexer(
		db,
		testLogger(),
		client.Context{TxConfig: testTxConfig{tx: decodedTx}},
		WithVirtualBankTransfers(true, chainID.String()),
	)

	height := int64(8)
	block := tmtypes.MakeBlock(height, []tmtypes.Tx{tmtypes.Tx("eth-tx")}, &tmtypes.Commit{Height: height - 1}, nil)
	block.Header.ValidatorsHash = common.HexToHash("0x08").Bytes()
	ethHash := ethMsg.Hash()
	blockResults := &coretypes.ResultBlockResults{
		Height: height,
		TxResults: []*abci.ExecTxResult{
			{
				Code:    abci.CodeTypeOK,
				GasUsed: 21000,
				Data: mustMarshalIndexerTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
					Hash: ethHash.Hex(),
				}),
				Events: []abci.Event{
					{
						Type: evmtypes.EventTypeEthereumTx,
						Attributes: []abci.EventAttribute{
							{Key: evmtypes.AttributeKeyEthereumTxHash, Value: ethHash.Hex()},
							{Key: evmtypes.AttributeKeyTxIndex, Value: "0"},
							{Key: evmtypes.AttributeKeyTxGasUsed, Value: "21000"},
						},
					},
				},
			},
		},
		FinalizeBlockEvents: []abci.Event{
			{
				Type: virtualbank.EventTypeCoinReceived,
				Attributes: []abci.EventAttribute{
					{Key: "receiver", Value: "0x4444444444444444444444444444444444444444"},
					{Key: "amount", Value: "6inj"},
					{Key: virtualbank.AttributeMode, Value: virtualbank.ModeBeginBlock},
				},
			},
		},
	}

	if err := kv.IndexBlockWithResults(block, blockResults); err != nil {
		t.Fatalf("IndexBlockWithResults returned error: %v", err)
	}

	indexedByHash, err := kv.GetByTxHash(ethHash)
	if err != nil {
		t.Fatalf("GetByTxHash returned error: %v", err)
	}
	if indexedByHash.EthTxIndex != 0 {
		t.Fatalf("tx result used visible RPC index: got %d want 0", indexedByHash.EthTxIndex)
	}

	indexedByEVMOrdinal, err := kv.GetByBlockAndIndex(height, 0)
	if err != nil {
		t.Fatalf("GetByBlockAndIndex with EVM ordinal returned error: %v", err)
	}
	if indexedByEVMOrdinal.EthTxIndex != 0 || indexedByEVMOrdinal.TxIndex != 0 {
		t.Fatalf("unexpected EVM ordinal lookup result: %#v", indexedByEVMOrdinal)
	}
	if _, err := kv.GetByBlockAndIndex(height, 1); err == nil {
		t.Fatal("expected visible RPC index to be absent from the EVM tx index collection")
	}

	beginRPC, err := kv.GetRPCTransactionByBlockAndIndex(height, 0)
	if err != nil {
		t.Fatalf("GetRPCTransactionByBlockAndIndex for begin block virtual tx returned error: %v", err)
	}
	if beginRPC.Hash != virtualbank.BeginBlockHash(height) {
		t.Fatalf("unexpected begin block RPC hash: got %s want %s", beginRPC.Hash.Hex(), virtualbank.BeginBlockHash(height).Hex())
	}

	rpcTx, err := kv.GetRPCTransactionByBlockAndIndex(height, 1)
	if err != nil {
		t.Fatalf("GetRPCTransactionByBlockAndIndex for EVM tx returned error: %v", err)
	}
	if rpcTx.Hash != ethHash {
		t.Fatalf("unexpected RPC tx hash: got %s want %s", rpcTx.Hash.Hex(), ethHash.Hex())
	}
	if rpcTx.TransactionIndex == nil || uint64(*rpcTx.TransactionIndex) != 1 {
		t.Fatalf("unexpected RPC transaction index: got %v want 1", rpcTx.TransactionIndex)
	}

	receipt, err := kv.GetReceiptByTxHash(ethHash)
	if err != nil {
		t.Fatalf("GetReceiptByTxHash returned error: %v", err)
	}
	receiptIndex, ok := receipt["transactionIndex"].(hexutil.Uint64)
	if !ok || uint64(receiptIndex) != 1 {
		t.Fatalf("unexpected receipt transaction index: got %#v want 1", receipt["transactionIndex"])
	}
}

func TestKVIndexerTraceCacheRoundTrip(t *testing.T) {
	db := dbm.NewMemDB()
	kv := &KVIndexer{db: db, logger: testLogger()}

	txHash := common.HexToHash("0xaa")
	height := int64(42)
	txPayload := []byte(`{"type":"call","gasUsed":"0x0"}`)
	blockPayload := []byte(`[{"result":{"type":"call"}}]`)

	if err := kv.SetTraceTransaction(txHash, nil, txPayload); err != nil {
		t.Fatalf("SetTraceTransaction returned error: %v", err)
	}
	if err := kv.SetTraceBlockByHeight(height, nil, blockPayload); err != nil {
		t.Fatalf("SetTraceBlockByHeight returned error: %v", err)
	}

	gotTx, err := kv.GetTraceTransaction(txHash, nil)
	if err != nil {
		t.Fatalf("GetTraceTransaction returned error: %v", err)
	}
	if string(gotTx) != string(txPayload) {
		t.Fatalf("unexpected tx trace payload: got %s want %s", gotTx, txPayload)
	}

	gotBlock, err := kv.GetTraceBlockByHeight(height, nil)
	if err != nil {
		t.Fatalf("GetTraceBlockByHeight returned error: %v", err)
	}
	if string(gotBlock) != string(blockPayload) {
		t.Fatalf("unexpected block trace payload: got %s want %s", gotBlock, blockPayload)
	}
}

type testSDKTx struct {
	msgs             []sdk.Msg
	extensionOptions []*codectypes.Any
}

// GetMsgs returns the messages supplied to the stub SDK transaction.
func (t testSDKTx) GetMsgs() []sdk.Msg {
	return t.msgs
}

// GetMsgsV2 satisfies the SDK transaction interface for tests that only need
// legacy message access.
func (t testSDKTx) GetMsgsV2() ([]protov2.Message, error) {
	return nil, nil
}

func (t testSDKTx) GetExtensionOptions() []*codectypes.Any {
	return t.extensionOptions
}

func (t testSDKTx) GetNonCriticalExtensionOptions() []*codectypes.Any {
	return nil
}

func evmExtensionOptions() []*codectypes.Any {
	return []*codectypes.Any{
		{TypeUrl: "/injective.evm.v1.ExtensionOptionsEthereumTx"},
	}
}

func mustMarshalIndexerTxMsgData(t *testing.T, responses ...*evmtypes.MsgEthereumTxResponse) []byte {
	t.Helper()

	msgResponses := make([]*codectypes.Any, 0, len(responses))
	for _, response := range responses {
		anyRsp, err := codectypes.NewAnyWithValue(response)
		if err != nil {
			t.Fatalf("NewAnyWithValue: %v", err)
		}
		msgResponses = append(msgResponses, anyRsp)
	}

	txMsgData := sdk.TxMsgData{MsgResponses: msgResponses}
	data, err := gogoproto.Marshal(&txMsgData)
	if err != nil {
		t.Fatalf("Marshal TxMsgData: %v", err)
	}
	return data
}

type testTxConfig struct {
	tx sdk.Tx
}

// TxEncoder returns a no-op encoder because the indexer tests inject decoded
// transactions directly.
func (c testTxConfig) TxEncoder() sdk.TxEncoder {
	return func(tx sdk.Tx) ([]byte, error) { return nil, nil }
}

// TxDecoder returns the stub transaction configured for the test.
func (c testTxConfig) TxDecoder() sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, error) { return c.tx, nil }
}

// TxJSONEncoder reuses the no-op binary encoder for unused test paths.
func (c testTxConfig) TxJSONEncoder() sdk.TxEncoder {
	return c.TxEncoder()
}

// TxJSONDecoder reuses the stub decoder for unused test paths.
func (c testTxConfig) TxJSONDecoder() sdk.TxDecoder {
	return c.TxDecoder()
}

// MarshalSignatureJSON satisfies the TxConfig interface for unused signature
// JSON paths.
func (c testTxConfig) MarshalSignatureJSON([]signing.SignatureV2) ([]byte, error) {
	return nil, nil
}

// UnmarshalSignatureJSON satisfies the TxConfig interface for unused signature
// JSON paths.
func (c testTxConfig) UnmarshalSignatureJSON([]byte) ([]signing.SignatureV2, error) {
	return nil, nil
}

// NewTxBuilder satisfies the TxConfig interface for paths that do not build
// transactions.
func (c testTxConfig) NewTxBuilder() client.TxBuilder {
	return nil
}

// WrapTxBuilder satisfies the TxConfig interface for paths that do not build
// transactions.
func (c testTxConfig) WrapTxBuilder(sdk.Tx) (client.TxBuilder, error) {
	return nil, nil
}

// SignModeHandler satisfies the TxConfig interface for unused signing paths.
func (c testTxConfig) SignModeHandler() *txsigning.HandlerMap {
	return nil
}

// SigningContext satisfies the TxConfig interface for unused signing paths.
func (c testTxConfig) SigningContext() *txsigning.Context {
	return nil
}
