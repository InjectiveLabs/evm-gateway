package indexer

import (
	"errors"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

func TestTxHashOwnerHeightLookupOrder(t *testing.T) {
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	height := mainnetfx.HeightEx1Included
	indexFixture(t, kv, height)
	hash := mainnetfx.TxEx1

	assertOwner := func(label string, wantHeight int64, wantFound bool) {
		t.Helper()
		owner, found, err := kv.txHashOwnerHeight(hash)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if found != wantFound || owner != wantHeight {
			t.Fatalf("%s: owner %d found %v want %d %v", label, owner, found, wantHeight, wantFound)
		}
	}

	assertOwner("tx result", height, true)

	// virtual txs have no tx result: the rpc tx payload owns the hash
	if err := db.Delete(TxHashKey(hash)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertOwner("rpc tx", height, true)

	if err := db.Delete(RPCtxHashKey(hash)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertOwner("receipt", height, true)

	if err := db.Delete(ReceiptKey(hash)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertOwner("missing", 0, false)

	owned, err := kv.txHashOwnedByHeight(hash, height+1)
	if err != nil || !owned {
		t.Fatalf("missing records must count as owned: %v %v", owned, err)
	}
}

func TestTxHashOwnerHeightIgnoresZeroHeightAndUndecodablePayloads(t *testing.T) {
	hash := common.HexToHash("0x01")
	zeroRPCTx := mustMarshalRPCTransaction(&rpctypes.RPCTransaction{BlockNumber: hexBig(0)})
	zeroTxResult := mustMarshalTxResult(&chaintypes.TxResult{Height: 0})
	zeroReceipt := mustMarshalReceipt(CachedReceipt{BlockNumber: 0})
	garbage := []byte("not a payload")

	for _, tc := range []struct {
		name    string
		records map[string][]byte
	}{
		{name: "zero heights", records: map[string][]byte{
			string(TxHashKey(hash)):    zeroTxResult,
			string(RPCtxHashKey(hash)): zeroRPCTx,
			string(ReceiptKey(hash)):   zeroReceipt,
		}},
		{name: "rpc tx without block number", records: map[string][]byte{
			string(RPCtxHashKey(hash)): mustMarshalRPCTransaction(&rpctypes.RPCTransaction{}),
		}},
		{name: "garbage", records: map[string][]byte{
			string(TxHashKey(hash)):    garbage,
			string(RPCtxHashKey(hash)): garbage,
			string(ReceiptKey(hash)):   garbage,
		}},
	} {
		db := dbm.NewMemDB()
		for key, value := range tc.records {
			if err := db.Set([]byte(key), value); err != nil {
				t.Fatalf("set: %v", err)
			}
		}
		kv := newFixtureIndexer(t, db)
		owner, found, err := kv.txHashOwnerHeight(hash)
		if err != nil || found || owner != 0 {
			t.Fatalf("%s: owner %d found %v err %v", tc.name, owner, found, err)
		}
	}
}

func TestTxHashOwnerHeightLegacyProtobufTxResult(t *testing.T) {
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	hash := common.HexToHash("0x02")
	legacy, err := clientCtx.Codec.Marshal(&chaintypes.TxResult{Height: 77, TxIndex: 1})
	if err != nil {
		t.Fatalf("marshal legacy tx result: %v", err)
	}
	db := dbm.NewMemDB()
	if err := db.Set(TxHashKey(hash), legacy); err != nil {
		t.Fatalf("set: %v", err)
	}

	withCodec := NewKVIndexer(db, testLogger(), clientCtx)
	owner, found, err := withCodec.txHashOwnerHeight(hash)
	if err != nil || !found || owner != 77 {
		t.Fatalf("legacy payload with codec: owner %d found %v err %v", owner, found, err)
	}

	// Without a codec the legacy decoder panics; the lookup must recover and
	// report the record as missing.
	withoutCodec := NewKVIndexer(db, testLogger(), client.Context{})
	owner, found, err = withoutCodec.txHashOwnerHeight(hash)
	if err != nil || found || owner != 0 {
		t.Fatalf("legacy payload without codec: owner %d found %v err %v", owner, found, err)
	}
}

func TestTxHashOwnerHeightPropagatesDBErrors(t *testing.T) {
	hash := common.HexToHash("0x03")
	for _, prefix := range []byte{KeyPrefixTxHash, KeyPrefixRPCtxHash, KeyPrefixReceipt} {
		kv := newFixtureIndexer(t, failingGetDB{DB: dbm.NewMemDB(), prefix: prefix})
		if _, _, err := kv.txHashOwnerHeight(hash); !errors.Is(err, errInjectedGet) {
			t.Fatalf("prefix %d: expected injected error, got %v", prefix, err)
		}
		if _, err := kv.txHashOwnedByHeight(hash, 1); !errors.Is(err, errInjectedGet) {
			t.Fatalf("prefix %d: expected injected error from owned check, got %v", prefix, err)
		}
	}
}

func TestDeleteTraceTxKeysRemovesEveryConfig(t *testing.T) {
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	hash := common.HexToHash("0x04")
	other := common.HexToHash("0x05")
	for _, h := range []common.Hash{hash, other} {
		for _, cfg := range []*rpctypes.TraceConfig{nil, {TracerConfig: []byte(`{"onlyTopCall":true}`)}} {
			if err := kv.SetTraceTransaction(h, cfg, []byte(`{}`)); err != nil {
				t.Fatalf("SetTraceTransaction: %v", err)
			}
		}
	}

	batch := db.NewBatch()
	if err := kv.deleteTraceTxKeys(batch, hash); err != nil {
		t.Fatalf("deleteTraceTxKeys: %v", err)
	}
	if err := batch.Write(); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = batch.Close()

	it, err := db.Iterator(traceTxPrefixStart(hash), prefixRangeEnd(traceTxPrefixStart(hash)))
	if err != nil {
		t.Fatalf("iterator: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected all traces of %s to be removed", hash.Hex())
	}
	_ = it.Close()
	mustGet(t, db, TraceTxKey(other, nil))

	// batch delete failures are reported
	if err := kv.deleteTraceTxKeys(failingDeleteBatch{}, other); err == nil {
		t.Fatalf("expected batch delete error")
	}
	if err := kv.deleteTraceKeysForBlock(failingDeleteBatch{}, 1, []common.Hash{other}); err == nil {
		t.Fatalf("expected batch delete error for block")
	}
}

type failingDeleteBatch struct{}

func (failingDeleteBatch) Delete([]byte) error { return errors.New("injected delete failure") }
