package backend

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

func TestOfflineCachedBlockAndTransactionLookups(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{OfflineRPCOnly: true},
		indexer: kv,
	}

	latest, err := b.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber returned error: %v", err)
	}
	if uint64(latest) != uint64(fixture.meta.Height) {
		t.Fatalf("unexpected latest block: got %d want %d", latest, fixture.meta.Height)
	}

	byNumber, err := b.GetBlockByNumber(rpctypes.EthLatestBlockNumber, false)
	if err != nil {
		t.Fatalf("GetBlockByNumber returned error: %v", err)
	}
	backendTestAssertBlockSummary(t, byNumber, fixture.meta, []common.Hash{fixture.txHashA, fixture.txHashB})

	byHash, err := b.GetBlockByHash(fixture.blockHash, true)
	if err != nil {
		t.Fatalf("GetBlockByHash returned error: %v", err)
	}
	fullTxs, ok := byHash["transactions"].([]interface{})
	if !ok {
		t.Fatalf("unexpected full tx payload type: %T", byHash["transactions"])
	}
	if len(fullTxs) != 2 {
		t.Fatalf("unexpected full tx count: got %d want 2", len(fullTxs))
	}
	firstTx, ok := fullTxs[0].(*rpctypes.RPCTransaction)
	if !ok {
		t.Fatalf("unexpected full tx entry type: %T", fullTxs[0])
	}
	if firstTx.Hash != fixture.txHashA {
		t.Fatalf("unexpected first tx hash: got %s want %s", firstTx.Hash.Hex(), fixture.txHashA.Hex())
	}

	header, err := b.HeaderByHash(fixture.blockHash)
	if err != nil {
		t.Fatalf("HeaderByHash returned error: %v", err)
	}
	if header.Number == nil || header.Number.Int64() != fixture.meta.Height {
		t.Fatalf("unexpected header number: got %v want %d", header.Number, fixture.meta.Height)
	}
	if header.TxHash != common.HexToHash(fixture.meta.TransactionsRoot) {
		t.Fatalf("unexpected header tx root: got %s want %s", header.TxHash.Hex(), fixture.meta.TransactionsRoot)
	}
	if header.BaseFee == nil || header.BaseFee.Cmp(big.NewInt(12345)) != 0 {
		t.Fatalf("unexpected header base fee: got %v want 12345", header.BaseFee)
	}

	txByNumber, err := b.GetTransactionByBlockNumberAndIndex(rpctypes.BlockNumber(fixture.meta.Height), 1)
	if err != nil {
		t.Fatalf("GetTransactionByBlockNumberAndIndex returned error: %v", err)
	}
	if txByNumber == nil || txByNumber.Hash != fixture.txHashB {
		t.Fatalf("unexpected tx by block number/index: got %#v want %s", txByNumber, fixture.txHashB.Hex())
	}

	txByHash, err := b.GetTransactionByBlockHashAndIndex(fixture.blockHash, 0)
	if err != nil {
		t.Fatalf("GetTransactionByBlockHashAndIndex returned error: %v", err)
	}
	if txByHash == nil || txByHash.Hash != fixture.txHashA {
		t.Fatalf("unexpected tx by block hash/index: got %#v want %s", txByHash, fixture.txHashA.Hex())
	}
}

func TestOfflineCachedBlockReceiptLookups(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{OfflineRPCOnly: true},
		indexer: kv,
	}

	blockNumber := rpctypes.BlockNumber(fixture.meta.Height)
	byNumber, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber})
	if err != nil {
		t.Fatalf("GetBlockReceipts by number returned error: %v", err)
	}
	backendTestAssertReceiptSummary(t, byNumber, fixture.meta.Height, fixture.blockHash, []common.Hash{fixture.txHashA, fixture.txHashB})

	latest := rpctypes.EthLatestBlockNumber
	byLatest, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &latest})
	if err != nil {
		t.Fatalf("GetBlockReceipts by latest returned error: %v", err)
	}
	backendTestAssertReceiptSummary(t, byLatest, fixture.meta.Height, fixture.blockHash, []common.Hash{fixture.txHashA, fixture.txHashB})

	byHash, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockHash: &fixture.blockHash})
	if err != nil {
		t.Fatalf("GetBlockReceipts by hash returned error: %v", err)
	}
	backendTestAssertReceiptSummary(t, byHash, fixture.meta.Height, fixture.blockHash, []common.Hash{fixture.txHashA, fixture.txHashB})
}

func TestOfflineCachedBlockLegacyMetaCompatibility(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	fixture.meta.StateRoot = ""
	fixture.meta.TransactionsRoot = ""
	fixture.meta.Miner = ""
	backendTestSetJSON(t, db, indexer.BlockMetaKey(fixture.meta.Height), fixture.meta)

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{OfflineRPCOnly: true},
		indexer: kv,
	}

	byNumber, err := b.GetBlockByNumber(rpctypes.BlockNumber(fixture.meta.Height), false)
	if err != nil {
		t.Fatalf("GetBlockByNumber returned error: %v", err)
	}
	if gotRoot, ok := byNumber["stateRoot"].(hexutil.Bytes); !ok || common.BytesToHash(gotRoot) != (common.Hash{}) {
		t.Fatalf("unexpected state root: got %#v want zero hash", byNumber["stateRoot"])
	}
	if gotRoot, ok := byNumber["transactionsRoot"].(common.Hash); !ok || gotRoot != (common.Hash{}) {
		t.Fatalf("unexpected tx root: got %#v want zero hash", byNumber["transactionsRoot"])
	}
	if gotMiner, ok := byNumber["miner"].(common.Address); !ok || gotMiner != (common.Address{}) {
		t.Fatalf("unexpected miner: got %#v want zero address", byNumber["miner"])
	}

	header, err := b.HeaderByHash(fixture.blockHash)
	if err != nil {
		t.Fatalf("HeaderByHash returned error: %v", err)
	}
	if header.Root != (common.Hash{}) {
		t.Fatalf("unexpected header state root: got %s want zero hash", header.Root.Hex())
	}
	if header.TxHash != (common.Hash{}) {
		t.Fatalf("unexpected header tx root: got %s want zero hash", header.TxHash.Hex())
	}
	if header.Coinbase != (common.Address{}) {
		t.Fatalf("unexpected header miner: got %s want zero address", header.Coinbase.Hex())
	}
}

func TestOfflineMissingTransactionCacheReturnsNil(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	if err := db.Delete(indexer.RPCtxHashKey(fixture.txHashA)); err != nil {
		t.Fatalf("delete cached rpc tx: %v", err)
	}
	if err := db.Delete(indexer.TxHashKey(fixture.txHashA)); err != nil {
		t.Fatalf("delete tx hash mapping: %v", err)
	}
	if err := db.Delete(indexer.ReceiptKey(fixture.txHashA)); err != nil {
		t.Fatalf("delete receipt cache: %v", err)
	}

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{OfflineRPCOnly: true},
		indexer: kv,
	}

	tx, err := b.GetTransactionByHash(fixture.txHashA)
	if err != nil {
		t.Fatalf("GetTransactionByHash returned error: %v", err)
	}
	if tx != nil {
		t.Fatalf("expected nil tx, got %#v", tx)
	}

	receipt, err := b.GetTransactionReceipt(fixture.txHashA)
	if err != nil {
		t.Fatalf("GetTransactionReceipt returned error: %v", err)
	}
	if receipt != nil {
		t.Fatalf("expected nil receipt, got %#v", receipt)
	}
}

func TestLiveModePrefersCachedBlockDataWhenAvailable(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{},
		indexer: kv,
	}

	byHash, err := b.GetBlockByHash(fixture.blockHash, true)
	if err != nil {
		t.Fatalf("GetBlockByHash returned error: %v", err)
	}
	fullTxs, ok := byHash["transactions"].([]interface{})
	if !ok || len(fullTxs) != 2 {
		t.Fatalf("unexpected full tx payload: %#v", byHash["transactions"])
	}

	byNumber, err := b.GetBlockByNumber(rpctypes.BlockNumber(fixture.meta.Height), false)
	if err != nil {
		t.Fatalf("GetBlockByNumber returned error: %v", err)
	}
	backendTestAssertBlockSummary(t, byNumber, fixture.meta, []common.Hash{fixture.txHashA, fixture.txHashB})

	header, err := b.HeaderByHash(fixture.blockHash)
	if err != nil {
		t.Fatalf("HeaderByHash returned error: %v", err)
	}
	if header == nil || header.Number == nil || header.Number.Int64() != fixture.meta.Height {
		t.Fatalf("unexpected header: %#v", header)
	}

	txByHash, err := b.GetTransactionByBlockHashAndIndex(fixture.blockHash, 1)
	if err != nil {
		t.Fatalf("GetTransactionByBlockHashAndIndex returned error: %v", err)
	}
	if txByHash == nil || txByHash.Hash != fixture.txHashB {
		t.Fatalf("unexpected tx by block hash/index: %#v", txByHash)
	}

	latest, err := b.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber returned error: %v", err)
	}
	if uint64(latest) != uint64(fixture.meta.Height) {
		t.Fatalf("unexpected live-mode cached latest: got %d want %d", latest, fixture.meta.Height)
	}
	byLatest, err := b.GetBlockByNumber(rpctypes.EthLatestBlockNumber, false)
	if err != nil {
		t.Fatalf("GetBlockByNumber(latest) returned error: %v", err)
	}
	backendTestAssertBlockSummary(t, byLatest, fixture.meta, []common.Hash{fixture.txHashA, fixture.txHashB})
}

func TestOfflineCachedBlockRejectsIncompleteTransactionCache(t *testing.T) {
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})
	fixture := backendTestSeedCachedBlockFixture(t, db)

	if err := db.Delete(indexer.RPCtxHashKey(fixture.txHashB)); err != nil {
		t.Fatalf("delete cached rpc tx: %v", err)
	}

	b := &Backend{
		logger:  backendTestLogger(),
		cfg:     appconfig.Config{OfflineRPCOnly: true},
		indexer: kv,
	}

	_, err := b.GetBlockByHash(fixture.blockHash, true)
	if err == nil {
		t.Fatal("expected GetBlockByHash to fail when cached block tx data is incomplete")
	}
	if !strings.Contains(err.Error(), "cache miss") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func backendTestAssertBlockSummary(t *testing.T, block map[string]interface{}, meta indexer.CachedBlockMeta, txHashes []common.Hash) {
	t.Helper()

	number, ok := block["number"].(hexutil.Uint64)
	if !ok {
		t.Fatalf("unexpected block number type: %T", block["number"])
	}
	if uint64(number) != uint64(meta.Height) {
		t.Fatalf("unexpected block number: got %d want %d", number, meta.Height)
	}

	hashBytes, ok := block["hash"].(hexutil.Bytes)
	if !ok {
		t.Fatalf("unexpected block hash type: %T", block["hash"])
	}
	if common.BytesToHash(hashBytes) != common.HexToHash(meta.Hash) {
		t.Fatalf("unexpected block hash: got %s want %s", common.BytesToHash(hashBytes).Hex(), meta.Hash)
	}

	if gotMiner, ok := block["miner"].(common.Address); !ok || gotMiner != common.HexToAddress(meta.Miner) {
		t.Fatalf("unexpected miner: got %#v want %s", block["miner"], meta.Miner)
	}
	if gotRoot, ok := block["transactionsRoot"].(common.Hash); !ok || gotRoot != common.HexToHash(meta.TransactionsRoot) {
		t.Fatalf("unexpected tx root: got %#v want %s", block["transactionsRoot"], meta.TransactionsRoot)
	}
	if gotBaseFee, ok := block["baseFeePerGas"].(*hexutil.Big); !ok || (*big.Int)(gotBaseFee).Cmp(big.NewInt(12345)) != 0 {
		t.Fatalf("unexpected base fee: got %#v want 12345", block["baseFeePerGas"])
	}

	txs, ok := block["transactions"].([]interface{})
	if !ok {
		t.Fatalf("unexpected tx list type: %T", block["transactions"])
	}
	if len(txs) != len(txHashes) {
		t.Fatalf("unexpected tx count: got %d want %d", len(txs), len(txHashes))
	}
	for i, want := range txHashes {
		got, ok := txs[i].(common.Hash)
		if !ok {
			t.Fatalf("unexpected tx hash entry type at %d: %T", i, txs[i])
		}
		if got != want {
			t.Fatalf("unexpected tx hash at %d: got %s want %s", i, got.Hex(), want.Hex())
		}
	}
}

func backendTestAssertReceiptSummary(t *testing.T, receipts []map[string]interface{}, blockHeight int64, blockHash common.Hash, txHashes []common.Hash) {
	t.Helper()

	if len(receipts) != len(txHashes) {
		t.Fatalf("unexpected receipt count: got %d want %d", len(receipts), len(txHashes))
	}

	for i, want := range txHashes {
		receipt := receipts[i]
		gotHash, ok := receipt["transactionHash"].(common.Hash)
		if !ok {
			t.Fatalf("unexpected transactionHash type at %d: %T", i, receipt["transactionHash"])
		}
		if gotHash != want {
			t.Fatalf("unexpected tx hash at %d: got %s want %s", i, gotHash.Hex(), want.Hex())
		}

		gotBlockHash, ok := receipt["blockHash"].(string)
		if !ok || gotBlockHash != blockHash.Hex() {
			t.Fatalf("unexpected block hash at %d: got %#v want %s", i, receipt["blockHash"], blockHash.Hex())
		}

		gotBlockNumber, ok := receipt["blockNumber"].(hexutil.Uint64)
		if !ok || uint64(gotBlockNumber) != uint64(blockHeight) {
			t.Fatalf("unexpected block number at %d: got %#v want %d", i, receipt["blockNumber"], blockHeight)
		}

		gotIndex, ok := receipt["transactionIndex"].(hexutil.Uint64)
		if !ok || uint64(gotIndex) != uint64(i) {
			t.Fatalf("unexpected transaction index at %d: got %#v want %d", i, receipt["transactionIndex"], i)
		}
	}
}

func backendTestRPCTx(hash, blockHash common.Hash, blockNumber uint64, index uint64) rpctypes.RPCTransaction {
	blockNum := (*hexutil.Big)(new(big.Int).SetUint64(blockNumber))
	txIndex := hexutil.Uint64(index)
	to := common.HexToAddress("0x00000000000000000000000000000000000000ff")

	return rpctypes.RPCTransaction{
		BlockHash:        &blockHash,
		BlockNumber:      blockNum,
		From:             common.HexToAddress("0x00000000000000000000000000000000000000aa"),
		Gas:              hexutil.Uint64(21000),
		GasPrice:         (*hexutil.Big)(big.NewInt(1)),
		Hash:             hash,
		Input:            hexutil.Bytes{},
		Nonce:            hexutil.Uint64(index),
		To:               &to,
		TransactionIndex: &txIndex,
		Value:            (*hexutil.Big)(big.NewInt(0)),
		Type:             hexutil.Uint64(ethtypes.LegacyTxType),
		ChainID:          (*hexutil.Big)(big.NewInt(1)),
		V:                (*hexutil.Big)(big.NewInt(27)),
		R:                (*hexutil.Big)(big.NewInt(1)),
		S:                (*hexutil.Big)(big.NewInt(2)),
	}
}

func backendTestSetJSON(t *testing.T, db dbm.DB, key []byte, value interface{}) {
	t.Helper()

	bz, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	backendTestSetRaw(t, db, key, bz)
}

func backendTestSetRaw(t *testing.T, db dbm.DB, key, value []byte) {
	t.Helper()
	if err := db.Set(key, value); err != nil {
		t.Fatalf("set %x: %v", key, err)
	}
}

func backendTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type backendCachedBlockFixture struct {
	meta      indexer.CachedBlockMeta
	blockHash common.Hash
	txHashA   common.Hash
	txHashB   common.Hash
}

func backendTestSeedCachedBlockFixture(t *testing.T, db dbm.DB) backendCachedBlockFixture {
	t.Helper()

	meta := indexer.CachedBlockMeta{
		Height:           42,
		Hash:             common.HexToHash("0x42").Hex(),
		ParentHash:       common.HexToHash("0x41").Hex(),
		StateRoot:        common.HexToHash("0x1234").Hex(),
		Miner:            common.HexToAddress("0x0000000000000000000000000000000000000042").Hex(),
		Timestamp:        1710000000,
		Size:             512,
		GasLimit:         30000000,
		GasUsed:          42000,
		EthTxCount:       2,
		TxCount:          3,
		Bloom:            hexutil.Encode(make([]byte, ethtypes.BloomByteLength)),
		TransactionsRoot: common.HexToHash("0xbeef").Hex(),
		BaseFee:          hexutil.EncodeBig(big.NewInt(12345)),
	}
	blockHash := common.HexToHash(meta.Hash)
	txHashA := common.HexToHash("0xaa")
	txHashB := common.HexToHash("0xbb")

	txA := backendTestRPCTx(txHashA, blockHash, uint64(meta.Height), 0)
	txB := backendTestRPCTx(txHashB, blockHash, uint64(meta.Height), 1)

	backendTestSetJSON(t, db, indexer.BlockMetaKey(meta.Height), meta)
	backendTestSetRaw(t, db, indexer.BlockHashKey(blockHash), sdk.Uint64ToBigEndian(uint64(meta.Height)))
	backendTestSetJSON(t, db, indexer.RPCtxHashKey(txHashA), txA)
	backendTestSetJSON(t, db, indexer.RPCtxHashKey(txHashB), txB)
	backendTestSetRaw(t, db, indexer.RPCtxIndexKey(meta.Height, 0), txHashA.Bytes())
	backendTestSetRaw(t, db, indexer.RPCtxIndexKey(meta.Height, 1), txHashB.Bytes())
	backendTestSetJSON(t, db, indexer.ReceiptKey(txHashA), indexer.CachedReceipt{
		Status:            uint64(ethtypes.ReceiptStatusSuccessful),
		CumulativeGasUsed: 21000,
		GasUsed:           21000,
		LogsBloom:         hexutil.Encode(make([]byte, ethtypes.BloomByteLength)),
		Logs:              []*ethtypes.Log{},
		TransactionHash:   txHashA.Hex(),
		BlockHash:         blockHash.Hex(),
		BlockNumber:       uint64(meta.Height),
		TransactionIndex:  0,
		EffectiveGasPrice: hexutil.EncodeBig(big.NewInt(1)),
		From:              txA.From.Hex(),
		To:                stringPtr(txA.To.Hex()),
		Type:              uint64(ethtypes.LegacyTxType),
	})
	backendTestSetJSON(t, db, indexer.ReceiptKey(txHashB), indexer.CachedReceipt{
		Status:            uint64(ethtypes.ReceiptStatusSuccessful),
		CumulativeGasUsed: 42000,
		GasUsed:           21000,
		LogsBloom:         hexutil.Encode(make([]byte, ethtypes.BloomByteLength)),
		Logs:              []*ethtypes.Log{},
		TransactionHash:   txHashB.Hex(),
		BlockHash:         blockHash.Hex(),
		BlockNumber:       uint64(meta.Height),
		TransactionIndex:  1,
		EffectiveGasPrice: hexutil.EncodeBig(big.NewInt(1)),
		From:              txB.From.Hex(),
		To:                stringPtr(txB.To.Hex()),
		Type:              uint64(ethtypes.LegacyTxType),
	})

	return backendCachedBlockFixture{
		meta:      meta,
		blockHash: blockHash,
		txHashA:   txHashA,
		txHashB:   txHashB,
	}
}

func stringPtr(v string) *string {
	return &v
}
