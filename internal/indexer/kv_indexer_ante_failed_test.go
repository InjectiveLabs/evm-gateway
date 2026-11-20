package indexer

import (
	"context"
	"encoding/json"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestIndexBlockSkipsAnteFailedEthTxs(t *testing.T) {
	testCases := []struct {
		height      int64
		wantSkipped int64
	}{
		{height: mainnetfx.HeightEx1Failed, wantSkipped: 2},
		{height: mainnetfx.HeightEx2Failed, wantSkipped: 8},
	}

	for _, tc := range testCases {
		db := dbm.NewMemDB()
		kv := newFixtureIndexer(t, db)
		stats := indexFixture(t, kv, tc.height)

		visible := mainnetfx.VisibleTxs[tc.height]
		if stats.SkippedAnteFailedEthTxs != tc.wantSkipped || stats.IndexedEthTxs != int64(len(visible)) {
			t.Fatalf("height %d: unexpected stats %+v", tc.height, stats)
		}
		if stats.ReassignedTxHashes != 0 || stats.SkippedDuplicateEthTxs != 0 {
			t.Fatalf("height %d: unexpected reassign/duplicate stats %+v", tc.height, stats)
		}
		assertBlockListing(t, kv, tc.height, visible)

		for _, failed := range mainnetfx.AnteFailedTxs[tc.height] {
			assertKeyMissing(t, db, TxHashKey(failed.Hash))
			assertKeyMissing(t, db, ReceiptKey(failed.Hash))
			assertKeyMissing(t, db, RPCtxHashKey(failed.Hash))
		}

		var prevCumulative uint64
		for i, hash := range visible {
			receipt := receiptAt(t, db, hash)
			txResult := txResultAt(t, kv, hash)
			if receipt.BlockNumber != uint64(tc.height) || receipt.TransactionIndex != uint64(i) {
				t.Fatalf("height %d tx %s: receipt at block %d index %d", tc.height, hash.Hex(), receipt.BlockNumber, receipt.TransactionIndex)
			}
			if receipt.Status != ethtypes.ReceiptStatusSuccessful || txResult.Failed {
				t.Fatalf("height %d tx %s: expected success", tc.height, hash.Hex())
			}
			if txResult.Height != tc.height || txResult.EthTxIndex != int32(i) || receipt.GasUsed != txResult.GasUsed {
				t.Fatalf("height %d tx %s: inconsistent tx result %+v receipt gas %d", tc.height, hash.Hex(), txResult, receipt.GasUsed)
			}
			if receipt.CumulativeGasUsed <= prevCumulative || receipt.CumulativeGasUsed < receipt.GasUsed {
				t.Fatalf("height %d tx %s: cumulative gas %d not increasing (prev %d)", tc.height, hash.Hex(), receipt.CumulativeGasUsed, prevCumulative)
			}
			prevCumulative = receipt.CumulativeGasUsed
		}
	}
}

func TestIndexBlockMessageExecutionFailureKeepsGasLimit(t *testing.T) {
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	stats := indexFixture(t, kv, mainnetfx.HeightEx2Included)
	if stats.SkippedAnteFailedEthTxs != 0 || stats.IndexedEthTxs != 5 {
		t.Fatalf("unexpected stats %+v", stats)
	}

	visible := mainnetfx.VisibleTxs[mainnetfx.HeightEx2Included]
	assertBlockListing(t, kv, mainnetfx.HeightEx2Included, visible)

	_, results := fixtureBlock(t, mainnetfx.HeightEx2Included)
	for i, hash := range visible[:4] {
		receipt := receiptAt(t, db, hash)
		txResult := txResultAt(t, kv, hash)
		// cumulativeGasUsed is the chain gas of the preceding Cosmos txs plus this
		// tx's gas limit; it is not the running sum of receipt gasUsed and drops
		// at the next receipt (see docs/gas-used-semantics.md).
		var cumulative uint64
		for _, prev := range results.TxResults[:txResult.TxIndex] {
			cumulative += uint64(prev.GasUsed)
		}
		cumulative += mainnetfx.GasLimitAnteFailed
		if receipt.Status != ethtypes.ReceiptStatusFailed || !txResult.Failed {
			t.Fatalf("tx %d: expected failed status", i)
		}
		// gas used semantics are unchanged: failures without evm events report
		// the gas limit (see docs/gas-used-semantics.md)
		if receipt.GasUsed != mainnetfx.GasLimitAnteFailed || txResult.GasUsed != mainnetfx.GasLimitAnteFailed {
			t.Fatalf("tx %d: gas used receipt %d tx result %d want %d", i, receipt.GasUsed, txResult.GasUsed, mainnetfx.GasLimitAnteFailed)
		}
		if txResult.CumulativeGasUsed != mainnetfx.GasLimitAnteFailed {
			t.Fatalf("tx %d: per-cosmos-tx cumulative gas %d", i, txResult.CumulativeGasUsed)
		}
		if receipt.CumulativeGasUsed != cumulative {
			t.Fatalf("tx %d: cumulative gas %d want %d", i, receipt.CumulativeGasUsed, cumulative)
		}
	}
	if receipt := receiptAt(t, db, visible[4]); receipt.Status != ethtypes.ReceiptStatusSuccessful {
		t.Fatalf("expected last tx to succeed")
	}
}

func TestIndexBlockReinclusionIsOrderIndependent(t *testing.T) {
	pairs := []struct {
		failed, included int64
		hash             common.Hash
		wantStatus       uint64
	}{
		{failed: mainnetfx.HeightEx1Failed, included: mainnetfx.HeightEx1Included, hash: mainnetfx.TxEx1, wantStatus: ethtypes.ReceiptStatusSuccessful},
		{failed: mainnetfx.HeightEx2Failed, included: mainnetfx.HeightEx2Included, hash: mainnetfx.TxEx2, wantStatus: ethtypes.ReceiptStatusFailed},
	}

	for _, pair := range pairs {
		var states []map[string]string
		for _, order := range [][]int64{{pair.failed, pair.included}, {pair.included, pair.failed}} {
			db := dbm.NewMemDB()
			kv := newFixtureIndexer(t, db)
			for _, height := range order {
				if stats := indexFixture(t, kv, height); stats.ReassignedTxHashes != 0 {
					t.Fatalf("order %v height %d: unexpected reassignment %+v", order, height, stats)
				}
			}

			if res := txResultAt(t, kv, pair.hash); res.Height != pair.included {
				t.Fatalf("order %v: tx owned by %d want %d", order, res.Height, pair.included)
			}
			receipt := receiptAt(t, db, pair.hash)
			if receipt.BlockNumber != uint64(pair.included) || receipt.Status != pair.wantStatus {
				t.Fatalf("order %v: receipt block %d status %d", order, receipt.BlockNumber, receipt.Status)
			}
			rpcTx, err := kv.GetRPCTransactionByHash(pair.hash)
			if err != nil || rpcTx.BlockNumber.ToInt().Int64() != pair.included {
				t.Fatalf("order %v: rpc tx %+v err %v", order, rpcTx, err)
			}
			assertBlockListing(t, kv, pair.failed, mainnetfx.VisibleTxs[pair.failed])
			assertBlockListing(t, kv, pair.included, mainnetfx.VisibleTxs[pair.included])
			states = append(states, dumpDB(t, db))
		}
		assertSameDB(t, states[0], states[1])
	}
}

func TestReindexOverLegacyStateReassignsStaleOwner(t *testing.T) {
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))

	// pre-fix state: the ante-failed inclusion owns the hash.
	if res := txResultAt(t, kv, mainnetfx.TxEx1); res.Height != mainnetfx.HeightEx1Failed || !res.Failed {
		t.Fatalf("legacy fixture: unexpected owner %+v", res)
	}
	traceCfg := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: "callTracer"}}
	for _, cfg := range []*rpctypes.TraceConfig{nil, traceCfg} {
		if err := kv.SetTraceTransaction(mainnetfx.TxEx1, cfg, json.RawMessage(`{"stale":true}`)); err != nil {
			t.Fatalf("SetTraceTransaction: %v", err)
		}
	}

	epoch := kv.CacheEpoch()
	stats := indexFixture(t, kv, mainnetfx.HeightEx1Included)
	if stats.ReassignedTxHashes != 1 || stats.IndexedEthTxs != 1 {
		t.Fatalf("unexpected stats %+v", stats)
	}
	if kv.CacheEpoch() == epoch {
		t.Fatalf("expected cache epoch to change")
	}

	if res := txResultAt(t, kv, mainnetfx.TxEx1); res.Height != mainnetfx.HeightEx1Included || res.Failed {
		t.Fatalf("unexpected owner after reindex %+v", res)
	}
	receipt := receiptAt(t, db, mainnetfx.TxEx1)
	if receipt.BlockNumber != uint64(mainnetfx.HeightEx1Included) || receipt.Status != ethtypes.ReceiptStatusSuccessful {
		t.Fatalf("unexpected receipt after reindex: block %d status %d", receipt.BlockNumber, receipt.Status)
	}
	for _, cfg := range []*rpctypes.TraceConfig{nil, traceCfg} {
		assertKeyMissing(t, db, TraceTxKey(mainnetfx.TxEx1, cfg))
	}
}

func TestDeleteBlockKeepsRecordsOwnedByAnotherHeight(t *testing.T) {
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db)
	owner := mainnetfx.HeightEx1Failed
	lister := mainnetfx.HeightEx1Included

	if err := kv.SetTraceTransaction(mainnetfx.TxEx1, nil, json.RawMessage(`{"type":"CALL"}`)); err != nil {
		t.Fatalf("SetTraceTransaction: %v", err)
	}
	if err := kv.SetTraceBlockByHeight(lister, nil, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("SetTraceBlockByHeight: %v", err)
	}

	epoch := kv.CacheEpoch()
	if err := kv.DeleteBlock(lister); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}
	if kv.CacheEpoch() == epoch {
		t.Fatalf("expected cache epoch to change")
	}

	// records owned by the other height are untouched
	for _, key := range [][]byte{
		TxHashKey(mainnetfx.TxEx1),
		ReceiptKey(mainnetfx.TxEx1),
		RPCtxHashKey(mainnetfx.TxEx1),
		TraceTxKey(mainnetfx.TxEx1, nil),
	} {
		mustGet(t, db, key)
	}
	if res := txResultAt(t, kv, mainnetfx.TxEx1); res.Height != owner {
		t.Fatalf("owner changed to %d", res.Height)
	}
	// the deleted height's own data is gone
	for _, key := range [][]byte{
		TxIndexKey(lister, 0),
		RPCtxIndexKey(lister, 0),
		BlockMetaKey(lister),
		BlockLogsKey(lister),
		TraceBlockKey(lister, nil),
	} {
		assertKeyMissing(t, db, key)
	}

	// deleting the owner height removes the hash records it owns
	if err := kv.DeleteBlock(owner); err != nil {
		t.Fatalf("DeleteBlock(owner): %v", err)
	}
	for _, key := range [][]byte{
		TxHashKey(mainnetfx.TxEx1),
		ReceiptKey(mainnetfx.TxEx1),
		RPCtxHashKey(mainnetfx.TxEx1),
		TraceTxKey(mainnetfx.TxEx1, nil),
	} {
		assertKeyMissing(t, db, key)
	}
}

func TestRepairLegacyStateMatchesFreshIndex(t *testing.T) {
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))

	var reassigned int64
	for _, height := range mainnetfx.Heights {
		reassigned += indexFixture(t, kv, height).ReassignedTxHashes
	}
	// Ascending order resets each stale ante-failed owner height first, which
	// drops the hash records it owns, so the later inclusions claim free hashes.
	if reassigned != 0 {
		t.Fatalf("reassigned %d hashes want 0", reassigned)
	}

	fresh := freshFixtureDB(t, mainnetfx.Heights...)
	assertSameDB(t, dumpDB(t, db, KeyPrefixMigration), dumpDB(t, fresh))
}

func TestRepairLegacyStateDescendingReassignsAndMatchesFreshIndex(t *testing.T) {
	db := loadLegacyState(t)
	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))

	var reassigned int64
	for i := len(mainnetfx.Heights) - 1; i >= 0; i-- {
		reassigned += indexFixture(t, kv, mainnetfx.Heights[i]).ReassignedTxHashes
	}
	// The later inclusions claim TxEx1 and the four ex2 hashes from the stale
	// ante-failed owners; resetting the owners afterwards keeps them.
	if reassigned != 5 {
		t.Fatalf("reassigned %d hashes want 5", reassigned)
	}

	fresh := freshFixtureDB(t, mainnetfx.Heights...)
	assertSameDB(t, dumpDB(t, db, KeyPrefixMigration), dumpDB(t, fresh))
}

func TestKVIndexerCacheEpoch(t *testing.T) {
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	clone := kv.WithContext(context.Background()).(*KVIndexer)

	if kv.CacheEpoch() != 0 {
		t.Fatalf("unexpected initial epoch %d", kv.CacheEpoch())
	}
	indexFixture(t, kv, mainnetfx.HeightEx1Failed)
	if kv.CacheEpoch() != 0 {
		t.Fatalf("indexing a new height must not change the epoch")
	}
	if err := kv.DeleteBlock(mainnetfx.HeightEx2Failed); err != nil {
		t.Fatalf("DeleteBlock(empty): %v", err)
	}
	if kv.CacheEpoch() != 0 {
		t.Fatalf("deleting an empty height must not change the epoch")
	}

	indexFixture(t, kv, mainnetfx.HeightEx1Failed)
	if kv.CacheEpoch() != 1 || clone.CacheEpoch() != 1 {
		t.Fatalf("reindex: epoch %d clone %d want 1", kv.CacheEpoch(), clone.CacheEpoch())
	}
	if err := clone.DeleteBlock(mainnetfx.HeightEx1Failed); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}
	if kv.CacheEpoch() != 2 {
		t.Fatalf("delete: epoch %d want 2", kv.CacheEpoch())
	}

	zero := &KVIndexer{db: db, logger: testLogger()}
	zero.bumpCacheEpoch()
	if zero.CacheEpoch() != 0 {
		t.Fatalf("zero-value indexer epoch %d", zero.CacheEpoch())
	}
}

func TestIndexBlockKeepsFirstDuplicateEthTx(t *testing.T) {
	block, results := fixtureBlock(t, mainnetfx.HeightEx1Included)
	ethTxIndex := -1
	for i, res := range results.TxResults {
		for _, event := range res.Events {
			if event.Type == evmtypes.EventTypeEthereumTx {
				ethTxIndex = i
			}
		}
	}
	if ethTxIndex < 0 {
		t.Fatalf("fixture has no ethereum tx")
	}

	dup := cmtypes.MakeBlock(block.Height, append(append([]cmtypes.Tx{}, block.Txs...), block.Txs[ethTxIndex]), block.LastCommit, nil)
	dup.Header = block.Header
	dupResults := *results
	dupResults.TxResults = append(append([]*abci.ExecTxResult{}, results.TxResults...), results.TxResults[ethTxIndex])

	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	stats, err := kv.IndexBlockWithStatsAndResults(dup, &dupResults)
	if err != nil {
		t.Fatalf("index duplicate block: %v", err)
	}
	if stats.SkippedDuplicateEthTxs != 1 || stats.IndexedEthTxs != 1 {
		t.Fatalf("unexpected stats %+v", stats)
	}
	assertBlockListing(t, kv, mainnetfx.HeightEx1Included, []common.Hash{mainnetfx.TxEx1})
	if res := txResultAt(t, kv, mainnetfx.TxEx1); res.TxIndex != uint32(ethTxIndex) {
		t.Fatalf("expected the first occurrence (tx %d), got %d", ethTxIndex, res.TxIndex)
	}
}

func TestIndexBlockOwnershipLookupErrors(t *testing.T) {
	block, results := fixtureBlock(t, mainnetfx.HeightEx1Included)
	db := failingGetDB{DB: dbm.NewMemDB(), prefix: KeyPrefixTxHash}
	kv := newFixtureIndexer(t, db)
	if _, err := kv.IndexBlockWithStatsAndResults(block, results); err == nil {
		t.Fatalf("expected ownership lookup error while claiming a tx hash")
	}

	// resetBlock ownership errors surface from DeleteBlock
	legacy := loadLegacyState(t)
	kv = newFixtureIndexer(t, failingGetDB{DB: legacy, prefix: KeyPrefixTxHash})
	if err := kv.DeleteBlock(mainnetfx.HeightEx1Included); err == nil {
		t.Fatalf("expected ownership lookup error in tx index loop")
	}

	// a block listed only in the rpc index (virtual txs) checks ownership there
	rpcOnly := dbm.NewMemDB()
	if err := rpcOnly.Set(RPCtxIndexKey(7, 0), common.HexToHash("0x77").Bytes()); err != nil {
		t.Fatalf("set: %v", err)
	}
	kv = newFixtureIndexer(t, failingGetDB{DB: rpcOnly, prefix: KeyPrefixTxHash})
	if err := kv.DeleteBlock(7); err == nil {
		t.Fatalf("expected ownership lookup error in rpc index loop")
	}
}
