package indexer

import (
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
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
	mustSet(RPCtxIndexKey(height, 0), txHashA.Bytes())
	mustSet(RPCtxIndexKey(height, 1), txHashB.Bytes())

	mustSet(BlockMetaKey(otherHeight), mustJSON(CachedBlockMeta{Height: otherHeight, Hash: otherBlockHash.Hex()}))
	mustSet(BlockHashKey(otherBlockHash), sdk.Uint64ToBigEndian(uint64(otherHeight)))
	mustSet(TxIndexKey(otherHeight, 0), txHashOther.Bytes())
	mustSet(TxHashKey(txHashOther), []byte("tx-other"))

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
	assertMissing(RPCtxIndexKey(height, 0))
	assertMissing(RPCtxIndexKey(height, 1))

	assertPresent(BlockMetaKey(otherHeight))
	assertPresent(BlockHashKey(otherBlockHash))
	assertPresent(TxIndexKey(otherHeight, 0))
	assertPresent(TxHashKey(txHashOther))
}
