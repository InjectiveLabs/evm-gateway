package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

const noFault = -1

// faultyDB injects failures by key prefix: Get, Iterator (by start key
// prefix) and batch Delete.
type faultyDB struct {
	dbm.DB
	getPrefix         int
	iteratorPrefix    int
	batchDeletePrefix int
}

func newFaultyDB(db dbm.DB) *faultyDB {
	return &faultyDB{DB: db, getPrefix: noFault, iteratorPrefix: noFault, batchDeletePrefix: noFault}
}

var errInjected = errors.New("injected failure")

func matchesPrefix(key []byte, prefix int) bool {
	return prefix != noFault && len(key) > 0 && int(key[0]) == prefix
}

func (db *faultyDB) Get(key []byte) ([]byte, error) {
	if matchesPrefix(key, db.getPrefix) {
		return nil, errInjected
	}
	return db.DB.Get(key)
}

func (db *faultyDB) Iterator(start, end []byte) (dbm.Iterator, error) {
	if matchesPrefix(start, db.iteratorPrefix) {
		return nil, errInjected
	}
	return db.DB.Iterator(start, end)
}

func (db *faultyDB) NewBatch() dbm.Batch {
	return faultyBatch{Batch: db.DB.NewBatch(), deletePrefix: db.batchDeletePrefix}
}

type faultyBatch struct {
	dbm.Batch
	deletePrefix int
}

func (b faultyBatch) Delete(key []byte) error {
	if matchesPrefix(key, b.deletePrefix) {
		return errInjected
	}
	return b.Batch.Delete(key)
}

// legacyStateWithCaches returns the legacy state plus cached traces for the
// hash owned by HeightEx1Failed and for its block.
func legacyStateWithCaches(t *testing.T) dbm.DB {
	t.Helper()
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db)
	if err := kv.SetTraceTransaction(mainnetfx.TxEx1, nil, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("SetTraceTransaction: %v", err)
	}
	for _, height := range []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx1Included} {
		if err := kv.SetTraceBlockByHeight(height, nil, json.RawMessage(`[]`)); err != nil {
			t.Fatalf("SetTraceBlockByHeight: %v", err)
		}
	}
	return db
}

func TestResetBlockBatchDeleteFailures(t *testing.T) {
	prefixes := []int{
		KeyPrefixBlockHash,
		KeyPrefixTxIndex,
		KeyPrefixTxHash,
		KeyPrefixReceipt,
		KeyPrefixRPCtxHash,
		KeyPrefixVirtualRPCtx,
		KeyPrefixRPCtxIndex,
		KeyPrefixTraceTx,
		KeyPrefixTraceBlock,
		KeyPrefixBlockLogs,
		KeyPrefixBlockMeta,
	}
	for _, prefix := range prefixes {
		db := newFaultyDB(legacyStateWithCaches(t))
		db.batchDeletePrefix = prefix
		kv := newFixtureIndexer(t, db)
		epoch := kv.CacheEpoch()
		if err := kv.DeleteBlock(mainnetfx.HeightEx1Failed); !errors.Is(err, errInjected) {
			t.Fatalf("prefix %d: expected injected delete failure, got %v", prefix, err)
		}
		if kv.CacheEpoch() != epoch {
			t.Fatalf("prefix %d: failed delete must not change the epoch", prefix)
		}
		// nothing was written
		mustGet(t, db.DB, BlockMetaKey(mainnetfx.HeightEx1Failed))
	}

	// owned hashes listed only in the rpc index (virtual txs)
	for _, prefix := range []int{KeyPrefixRPCtxIndex, KeyPrefixTxHash} {
		rpcOnly := dbm.NewMemDB()
		if err := rpcOnly.Set(RPCtxIndexKey(7, 0), common.HexToHash("0x77").Bytes()); err != nil {
			t.Fatalf("set: %v", err)
		}
		db := newFaultyDB(rpcOnly)
		db.batchDeletePrefix = prefix
		if err := newFixtureIndexer(t, db).DeleteBlock(7); !errors.Is(err, errInjected) {
			t.Fatalf("rpc-only prefix %d: expected injected delete failure, got %v", prefix, err)
		}
	}
}

func TestResetBlockReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*faultyDB)
	}{
		{name: "meta get", fault: func(db *faultyDB) { db.getPrefix = KeyPrefixBlockMeta }},
		{name: "tx index iterator", fault: func(db *faultyDB) { db.iteratorPrefix = KeyPrefixTxIndex }},
		{name: "rpc index iterator", fault: func(db *faultyDB) { db.iteratorPrefix = KeyPrefixRPCtxIndex }},
		{name: "trace tx iterator", fault: func(db *faultyDB) { db.iteratorPrefix = KeyPrefixTraceTx }},
		{name: "trace block iterator", fault: func(db *faultyDB) { db.iteratorPrefix = KeyPrefixTraceBlock }},
	} {
		db := newFaultyDB(legacyStateWithCaches(t))
		tc.fault(db)
		if err := newFixtureIndexer(t, db).DeleteBlock(mainnetfx.HeightEx1Failed); !errors.Is(err, errInjected) {
			t.Fatalf("%s: expected injected failure, got %v", tc.name, err)
		}
	}

	db := loadLegacyState(t)
	if err := db.Set(BlockMetaKey(mainnetfx.HeightEx1Failed), []byte("garbage")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := newFixtureIndexer(t, db).DeleteBlock(mainnetfx.HeightEx1Failed); err == nil {
		t.Fatalf("expected meta decode failure")
	}
}

func TestIndexBlockPropagatesResetAndClaimFailures(t *testing.T) {
	block, results := fixtureBlock(t, mainnetfx.HeightEx1Included)

	db := newFaultyDB(legacyStateWithCaches(t))
	db.getPrefix = KeyPrefixBlockMeta
	if _, err := newFixtureIndexer(t, db).IndexBlockWithStatsAndResults(block, results); !errors.Is(err, errInjected) {
		t.Fatalf("expected reset failure, got %v", err)
	}

	// reassigning TxEx1 from the stale owner drops its cached traces
	db = newFaultyDB(legacyStateWithCaches(t))
	db.batchDeletePrefix = KeyPrefixTraceTx
	kv := newFixtureIndexer(t, db)
	if _, err := kv.IndexBlockWithStatsAndResults(block, results); !errors.Is(err, errInjected) {
		t.Fatalf("expected claim trace delete failure, got %v", err)
	}
	if res := txResultAt(t, kv, mainnetfx.TxEx1); res.Height != mainnetfx.HeightEx1Failed {
		t.Fatalf("failed index must not write: owner %d", res.Height)
	}
}

func TestScanAnteFailedCandidatesIteratorFailures(t *testing.T) {
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	for _, prefix := range []int{KeyPrefixTxHash, KeyPrefixTxIndex} {
		db := newFaultyDB(loadLegacyState(t))
		db.iteratorPrefix = prefix
		_, err := ScanAnteFailedCandidates(context.Background(), db, clientCtx.Codec, AnteFailedScanOptions{}, nil)
		if !errors.Is(err, errInjected) {
			t.Fatalf("prefix %d: expected iterator failure, got %v", prefix, err)
		}
	}
}

func TestDeleteTraceKeysForBlockWrapsTxFailures(t *testing.T) {
	db := newFaultyDB(dbm.NewMemDB())
	db.iteratorPrefix = KeyPrefixTraceTx
	kv := newFixtureIndexer(t, db)
	err := kv.deleteTraceKeysForBlock(db.NewBatch(), 9, []common.Hash{common.HexToHash("0x09")})
	if !errors.Is(err, errInjected) || err.Error() == errInjected.Error() {
		t.Fatalf("expected wrapped iterator failure, got %v", err)
	}
	if got := fmt.Sprint(err); got == "" {
		t.Fatalf("empty error")
	}
}
