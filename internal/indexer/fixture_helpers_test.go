package indexer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

// legacyGasLimit returns the block gas limit cached by the pre-fix indexer in
// the legacy fixture, so re-indexed block metas are byte-identical.
func legacyGasLimit(t *testing.T) uint64 {
	t.Helper()
	db := loadLegacyState(t)
	meta, err := unmarshalBlockMetaPayload(mustGet(t, db, BlockMetaKey(mainnetfx.HeightEx1Failed)))
	if err != nil {
		t.Fatalf("decode legacy meta: %v", err)
	}
	return meta.GasLimit
}

func newFixtureIndexer(t *testing.T, db dbm.DB, opts ...KVIndexerOption) *KVIndexer {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	return NewKVIndexer(db, testLogger(), clientCtx, opts...)
}

func fixtureBlock(t *testing.T, height int64) (*cmtypes.Block, *coretypes.ResultBlockResults) {
	t.Helper()
	block, err := mainnetfx.Block(height)
	if err != nil {
		t.Fatalf("Block(%d): %v", height, err)
	}
	results, err := mainnetfx.BlockResults(height)
	if err != nil {
		t.Fatalf("BlockResults(%d): %v", height, err)
	}
	return block, results
}

func indexFixture(t *testing.T, kv *KVIndexer, height int64) BlockIndexStats {
	t.Helper()
	block, results := fixtureBlock(t, height)
	stats, err := kv.IndexBlockWithStatsAndResults(block, results)
	if err != nil {
		t.Fatalf("index block %d: %v", height, err)
	}
	return stats
}

func loadLegacyState(t *testing.T) dbm.DB {
	t.Helper()
	pairs, err := mainnetfx.LegacyReporterState()
	if err != nil {
		t.Fatalf("LegacyReporterState: %v", err)
	}
	db := dbm.NewMemDB()
	for _, pair := range pairs {
		if err := db.Set(pair.Key, pair.Value); err != nil {
			t.Fatalf("set legacy key: %v", err)
		}
	}
	return db
}

// freshFixtureDB indexes the given heights with the current indexer.
func freshFixtureDB(t *testing.T, heights ...int64) dbm.DB {
	t.Helper()
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))
	for _, height := range heights {
		indexFixture(t, kv, height)
	}
	return db
}

// dumpDB returns every key/value of db, skipping keys with excluded prefixes.
func dumpDB(t *testing.T, db dbm.DB, excludePrefixes ...byte) map[string]string {
	t.Helper()
	it, err := db.Iterator(nil, nil)
	if err != nil {
		t.Fatalf("iterator: %v", err)
	}
	defer it.Close()
	out := make(map[string]string)
outer:
	for ; it.Valid(); it.Next() {
		for _, prefix := range excludePrefixes {
			if len(it.Key()) > 0 && it.Key()[0] == prefix {
				continue outer
			}
		}
		out[hex.EncodeToString(it.Key())] = hex.EncodeToString(it.Value())
	}
	return out
}

func assertSameDB(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		gotValue, ok := got[key]
		if !ok {
			t.Fatalf("missing key %s", key)
		}
		if gotValue != value {
			t.Fatalf("key %s: value differs\n got %s\nwant %s", key, gotValue, value)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected key %s", key)
		}
	}
}

func mustGet(t *testing.T, db dbm.DB, key []byte) []byte {
	t.Helper()
	bz, err := db.Get(key)
	if err != nil {
		t.Fatalf("get %x: %v", key, err)
	}
	if len(bz) == 0 {
		t.Fatalf("expected key %x to be present", key)
	}
	return bz
}

func assertKeyMissing(t *testing.T, db dbm.DB, key []byte) {
	t.Helper()
	bz, err := db.Get(key)
	if err != nil {
		t.Fatalf("get %x: %v", key, err)
	}
	if len(bz) != 0 {
		t.Fatalf("expected key %x to be missing", key)
	}
}

func txResultAt(t *testing.T, kv *KVIndexer, hash common.Hash) *chaintypes.TxResult {
	t.Helper()
	res, err := kv.GetByTxHash(hash)
	if err != nil {
		t.Fatalf("GetByTxHash(%s): %v", hash.Hex(), err)
	}
	return res
}

func receiptAt(t *testing.T, db dbm.DB, hash common.Hash) CachedReceipt {
	t.Helper()
	receipt, err := unmarshalReceiptPayload(mustGet(t, db, ReceiptKey(hash)))
	if err != nil {
		t.Fatalf("decode receipt %s: %v", hash.Hex(), err)
	}
	return receipt
}

// assertBlockListing checks both per-block indexes list exactly want, at
// contiguous indexes starting from zero.
func assertBlockListing(t *testing.T, kv *KVIndexer, height int64, want []common.Hash) {
	t.Helper()
	hashes, err := kv.GetRPCTransactionHashesByBlockHeight(height)
	if err != nil {
		t.Fatalf("GetRPCTransactionHashesByBlockHeight(%d): %v", height, err)
	}
	if len(hashes) != len(want) {
		t.Fatalf("height %d: listing %d hashes want %d", height, len(hashes), len(want))
	}
	for i, hash := range want {
		if hashes[i] != hash {
			t.Fatalf("height %d listing[%d]: %s want %s", height, i, hashes[i].Hex(), hash.Hex())
		}
		if !bytes.Equal(mustGet(t, kv.db, TxIndexKey(height, int32(i))), hash.Bytes()) {
			t.Fatalf("height %d tx index %d does not map to %s", height, i, hash.Hex())
		}
		if !bytes.Equal(mustGet(t, kv.db, RPCtxIndexKey(height, int32(i))), hash.Bytes()) {
			t.Fatalf("height %d rpc tx index %d does not map to %s", height, i, hash.Hex())
		}
	}
	assertKeyMissing(t, kv.db, TxIndexKey(height, int32(len(want))))
	assertKeyMissing(t, kv.db, RPCtxIndexKey(height, int32(len(want))))
	meta, err := kv.GetBlockMetaByHeight(height)
	if err != nil {
		t.Fatalf("GetBlockMetaByHeight(%d): %v", height, err)
	}
	if meta.EthTxCount != int32(len(want)) {
		t.Fatalf("height %d: meta eth tx count %d want %d", height, meta.EthTxCount, len(want))
	}
}

// failingGetDB fails Get for keys starting with prefix.
type failingGetDB struct {
	dbm.DB
	prefix byte
}

var errInjectedGet = errors.New("injected get failure")

func (db failingGetDB) Get(key []byte) ([]byte, error) {
	if len(key) > 0 && key[0] == db.prefix {
		return nil, errInjectedGet
	}
	return db.DB.Get(key)
}
