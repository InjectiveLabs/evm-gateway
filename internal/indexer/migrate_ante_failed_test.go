package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

// fixtureFetcher serves block results from a static map.
type fixtureFetcher struct {
	mu      sync.Mutex
	results map[int64]*coretypes.ResultBlockResults
	errAt   map[int64]error
	fetched []int64
}

func newFixtureFetcher(t *testing.T) *fixtureFetcher {
	t.Helper()
	all, err := mainnetfx.AllBlockResults()
	if err != nil {
		t.Fatalf("AllBlockResults: %v", err)
	}
	return &fixtureFetcher{results: all, errAt: map[int64]error{}}
}

func (f *fixtureFetcher) BlockResults(ctx context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.fetched = append(f.fetched, *height)
	f.mu.Unlock()
	if err := f.errAt[*height]; err != nil {
		return nil, err
	}
	res, ok := f.results[*height]
	if !ok {
		return nil, fmt.Errorf("no fixture for height %d", *height)
	}
	return res, nil
}

func legacyCandidates() []AnteFailedCandidate {
	out := make([]AnteFailedCandidate, 0)
	for _, height := range mainnetfx.Heights {
		for _, failed := range mainnetfx.AnteFailedTxs[height] {
			out = append(out, AnteFailedCandidate{
				Height:   height,
				TxIndex:  failed.TxIndex,
				Hash:     failed.Hash,
				GasUsed:  mainnetfx.GasLimitAnteFailed,
				GasLimit: mainnetfx.GasLimitAnteFailed,
			})
		}
	}
	return out
}

func scanLegacy(t *testing.T, db dbm.DB, opts AnteFailedScanOptions) *AnteFailedScanResult {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	scan, err := ScanAnteFailedCandidates(context.Background(), db, clientCtx.Codec, opts, testLogger())
	if err != nil {
		t.Fatalf("ScanAnteFailedCandidates: %v", err)
	}
	return scan
}

func TestScanAnteFailedCandidatesOnLegacyState(t *testing.T) {
	scan := scanLegacy(t, loadLegacyState(t), AnteFailedScanOptions{})

	if scan.ScannedTxs != 17 || scan.FailedTxs != 10 {
		t.Fatalf("unexpected scan counters: %+v", scan)
	}
	want := legacyCandidates()
	if len(scan.Candidates) != len(want) {
		t.Fatalf("candidates %d want %d: %+v", len(scan.Candidates), len(want), scan.Candidates)
	}
	for i := range want {
		if scan.Candidates[i] != want[i] {
			t.Fatalf("candidate[%d] %+v want %+v", i, scan.Candidates[i], want[i])
		}
	}

	wantConflicts := []TxHashConflict{{Height: mainnetfx.HeightEx1Included, Hash: mainnetfx.TxEx1, OwnerHeight: mainnetfx.HeightEx1Failed}}
	for _, hash := range mainnetfx.VisibleTxs[mainnetfx.HeightEx2Included][:4] {
		wantConflicts = append(wantConflicts, TxHashConflict{Height: mainnetfx.HeightEx2Included, Hash: hash, OwnerHeight: mainnetfx.HeightEx2Failed})
	}
	if len(scan.Conflicts) != len(wantConflicts) {
		t.Fatalf("conflicts %+v want %+v", scan.Conflicts, wantConflicts)
	}
	for i := range wantConflicts {
		if scan.Conflicts[i] != wantConflicts[i] {
			t.Fatalf("conflict[%d] %+v want %+v", i, scan.Conflicts[i], wantConflicts[i])
		}
	}

	assertHeights(t, "candidate heights", scan.CandidateHeights(), []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx2Failed})
	assertHeights(t, "conflict heights", scan.ConflictHeights(), mainnetfx.Heights)
}

func TestScanAnteFailedCandidatesHeightBounds(t *testing.T) {
	db := loadLegacyState(t)
	for _, tc := range []struct {
		name           string
		opts           AnteFailedScanOptions
		wantCandidates int
		wantConflicts  int
	}{
		{name: "from", opts: AnteFailedScanOptions{From: mainnetfx.HeightEx1Included + 1}, wantCandidates: 8, wantConflicts: 4},
		{name: "to", opts: AnteFailedScanOptions{To: mainnetfx.HeightEx1Included}, wantCandidates: 2, wantConflicts: 1},
		{name: "single height", opts: AnteFailedScanOptions{From: mainnetfx.HeightEx2Included, To: mainnetfx.HeightEx2Included}, wantCandidates: 0, wantConflicts: 4},
		{name: "empty range", opts: AnteFailedScanOptions{From: 1, To: 2}, wantCandidates: 0, wantConflicts: 0},
	} {
		scan := scanLegacy(t, db, tc.opts)
		if len(scan.Candidates) != tc.wantCandidates || len(scan.Conflicts) != tc.wantConflicts {
			t.Fatalf("%s: candidates %d conflicts %d want %d %d", tc.name, len(scan.Candidates), len(scan.Conflicts), tc.wantCandidates, tc.wantConflicts)
		}
		for _, candidate := range scan.Candidates {
			if !tc.opts.contains(candidate.Height) {
				t.Fatalf("%s: candidate outside bounds %+v", tc.name, candidate)
			}
		}
	}
}

func TestScanAnteFailedCandidatesRecordShapes(t *testing.T) {
	// A fresh index never holds candidates or conflicts: message execution
	// failures store the chain gas used, not the gas limit.
	fresh := freshFixtureDB(t, mainnetfx.Heights...)
	scan := scanLegacy(t, fresh, AnteFailedScanOptions{})
	if len(scan.Candidates) != 0 || len(scan.Conflicts) != 0 || scan.FailedTxs != 4 {
		t.Fatalf("fresh index: unexpected scan %+v", scan)
	}

	// Missing rpc tx payload: the record can't be ruled out.
	db := loadLegacyState(t)
	if err := db.Delete(RPCtxHashKey(mainnetfx.TxEx1)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Malformed keys and undecodable payloads are skipped.
	short := common.HexToHash("0xab")
	for key, value := range map[string][]byte{
		string([]byte{KeyPrefixTxHash, 0xaa}):  []byte("x"),
		string(TxHashKey(short)):               []byte("garbage"),
		string([]byte{KeyPrefixTxIndex, 0x01}): short.Bytes(),
	} {
		if err := db.Set([]byte(key), value); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	// A listing whose hash has no (decodable) record is a conflict with owner 0.
	dangling := common.HexToHash("0xdd")
	if err := db.Set(TxIndexKey(5, 0), dangling.Bytes()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := db.Set(TxIndexKey(6, 0), short.Bytes()); err != nil {
		t.Fatalf("set: %v", err)
	}

	scan = scanLegacy(t, db, AnteFailedScanOptions{})
	var ex1 *AnteFailedCandidate
	for i := range scan.Candidates {
		if scan.Candidates[i].Hash == mainnetfx.TxEx1 {
			ex1 = &scan.Candidates[i]
		}
	}
	if len(scan.Candidates) != 10 || ex1 == nil || ex1.GasLimit != 0 {
		t.Fatalf("expected TxEx1 candidate without known gas limit: %+v", scan.Candidates)
	}
	if scan.ScannedTxs != 18 {
		t.Fatalf("scanned %d tx records want 18 (malformed key skipped)", scan.ScannedTxs)
	}
	owners := map[int64]int64{}
	for _, conflict := range scan.Conflicts {
		owners[conflict.Height] = conflict.OwnerHeight
	}
	if owner, ok := owners[5]; !ok || owner != 0 {
		t.Fatalf("expected dangling listing conflict at 5: %+v", scan.Conflicts)
	}
	if owner, ok := owners[6]; !ok || owner != 0 {
		t.Fatalf("expected undecodable owner conflict at 6: %+v", scan.Conflicts)
	}
	for _, height := range scan.ConflictHeights() {
		if height == 0 {
			t.Fatalf("owner height 0 must be excluded from conflict heights")
		}
	}

	// rpc tx payload that can't be decoded: gas limit unknown, still reported
	if err := db.Set(RPCtxHashKey(mainnetfx.TxEx2), []byte("garbage")); err != nil {
		t.Fatalf("set: %v", err)
	}
	scan = scanLegacy(t, db, AnteFailedScanOptions{})
	if len(scan.Candidates) != 10 {
		t.Fatalf("expected 10 candidates, got %d", len(scan.Candidates))
	}
}

func TestScanAnteFailedCandidatesSkipsNonFailedAndGasMismatch(t *testing.T) {
	db := dbm.NewMemDB()
	ok := common.HexToHash("0x01")
	mismatch := common.HexToHash("0x02")
	set := func(key, value []byte) {
		if err := db.Set(key, value); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	set(TxHashKey(ok), mustMarshalTxResult(&chaintypes.TxResult{Height: 10, GasUsed: 21000}))
	set(TxHashKey(mismatch), mustMarshalTxResult(&chaintypes.TxResult{Height: 10, Failed: true, GasUsed: 21000}))
	set(RPCtxHashKey(mismatch), mustMarshalRPCTransaction(&rpctypes.RPCTransaction{BlockNumber: hexBig(10), Gas: 50000}))

	scan := scanLegacy(t, db, AnteFailedScanOptions{})
	if scan.ScannedTxs != 2 || scan.FailedTxs != 1 || len(scan.Candidates) != 0 {
		t.Fatalf("unexpected scan %+v", scan)
	}
}

func TestScanAnteFailedCandidatesErrors(t *testing.T) {
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ScanAnteFailedCandidates(cancelled, loadLegacyState(t), clientCtx.Codec, AnteFailedScanOptions{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation in tx scan, got %v", err)
	}

	listingsOnly := dbm.NewMemDB()
	if err := listingsOnly.Set(TxIndexKey(1, 0), common.HexToHash("0x01").Bytes()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := ScanAnteFailedCandidates(cancelled, listingsOnly, clientCtx.Codec, AnteFailedScanOptions{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation in listing scan, got %v", err)
	}

	if _, err := ScanAnteFailedCandidates(context.Background(), failingGetDB{DB: loadLegacyState(t), prefix: KeyPrefixRPCtxHash}, clientCtx.Codec, AnteFailedScanOptions{}, nil); !errors.Is(err, errInjectedGet) {
		t.Fatalf("expected rpc tx lookup error, got %v", err)
	}
	if _, err := ScanAnteFailedCandidates(context.Background(), failingGetDB{DB: loadLegacyState(t), prefix: KeyPrefixTxHash}, clientCtx.Codec, AnteFailedScanOptions{}, nil); !errors.Is(err, errInjectedGet) {
		t.Fatalf("expected tx record lookup error, got %v", err)
	}
}

func TestVerifyAnteFailedCandidatesConfirmsLegacyCandidates(t *testing.T) {
	fetcher := newFixtureFetcher(t)
	verification, err := VerifyAnteFailedCandidates(context.Background(), fetcher, legacyCandidates(), 3, testLogger())
	if err != nil {
		t.Fatalf("VerifyAnteFailedCandidates: %v", err)
	}
	if verification.CheckedHeights != 2 || len(verification.Confirmed) != 10 {
		t.Fatalf("unexpected verification %+v", verification)
	}
	want := legacyCandidates()
	for i, confirmed := range verification.Confirmed {
		if confirmed.AnteFailedCandidate != want[i] || confirmed.Verdict != VerdictAnteFailed {
			t.Fatalf("confirmed[%d] %+v", i, confirmed)
		}
		if confirmed.Code != 5 || confirmed.Codespace != "sdk" || !strings.Contains(confirmed.Log, "insufficient funds") {
			t.Fatalf("confirmed[%d] unexpected result %d %s %q", i, confirmed.Code, confirmed.Codespace, confirmed.Log)
		}
	}
	assertHeights(t, "verified heights", verification.Heights(), []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx2Failed})
	if len(fetcher.fetched) != 2 {
		t.Fatalf("fetched %v: only candidate heights must be fetched", fetcher.fetched)
	}
}

func TestVerifyAnteFailedCandidatesVerdicts(t *testing.T) {
	const height = 1000
	fetcher := &fixtureFetcher{errAt: map[int64]error{}, results: map[int64]*coretypes.ResultBlockResults{
		height: {Height: height, TxResults: []*abci.ExecTxResult{
			{Code: 0, GasUsed: 21000},
			{Code: 3, Codespace: "evm", GasUsed: 30000},
			{Code: 11, Codespace: "sdk", Log: rpctypes.ExceedBlockGasLimitError + " 50000", GasUsed: 0},
			{Code: 1, Codespace: "undefined", Log: "failed to execute message; message index: 0: boom", GasUsed: 28209},
			{Code: 1, Codespace: "undefined", Log: "failed to execute message; message index: 0: boom", GasUsed: 28209},
			{Code: 32, Codespace: "sdk", Log: "invalid nonce; got 1, expected 2: " + strings.Repeat("x", 300)},
		}},
	}}
	candidate := func(txIndex uint32, gasUsed uint64) AnteFailedCandidate {
		return AnteFailedCandidate{Height: height, TxIndex: txIndex, Hash: common.BigToHash(common.Big1), GasUsed: gasUsed, GasLimit: 50000}
	}
	verification, err := VerifyAnteFailedCandidates(context.Background(), fetcher, []AnteFailedCandidate{
		candidate(0, 50000),
		candidate(1, 50000),
		candidate(2, 50000),
		candidate(3, 50000), // stored gas limit, chain gas used differs
		candidate(4, 28209), // already the chain gas used
		candidate(5, 50000),
	}, 0, nil)
	if err != nil {
		t.Fatalf("VerifyAnteFailedCandidates: %v", err)
	}
	if len(verification.Confirmed) != 2 {
		t.Fatalf("unexpected confirmed %+v", verification.Confirmed)
	}
	if got := verification.Confirmed[0]; got.TxIndex != 3 || got.Verdict != VerdictGasUsedMismatch {
		t.Fatalf("expected gas mismatch verdict, got %+v", got)
	}
	if got := verification.Confirmed[1]; got.TxIndex != 5 || got.Verdict != VerdictAnteFailed || len(got.Log) != 256+len("...") {
		t.Fatalf("expected ante failed verdict with truncated log, got %+v", got)
	}

	// the mainnet re-included tx stored with the legacy gas limit
	mainnet := newFixtureFetcher(t)
	verification, err = VerifyAnteFailedCandidates(context.Background(), mainnet, []AnteFailedCandidate{
		{Height: mainnetfx.HeightEx2Included, TxIndex: 1, Hash: mainnetfx.TxEx2, GasUsed: mainnetfx.GasLimitAnteFailed},
	}, 1, nil)
	if err != nil || len(verification.Confirmed) != 1 || verification.Confirmed[0].Verdict != VerdictGasUsedMismatch {
		t.Fatalf("expected gas mismatch on mainnet re-inclusion: %+v %v", verification, err)
	}
}

func TestVerifyAnteFailedCandidatesErrors(t *testing.T) {
	candidates := legacyCandidates()

	fetcher := newFixtureFetcher(t)
	fetcher.errAt[mainnetfx.HeightEx2Failed] = errors.New("node down")
	if _, err := VerifyAnteFailedCandidates(context.Background(), fetcher, candidates, 2, nil); err == nil || !strings.Contains(err.Error(), "node down") {
		t.Fatalf("expected fetch error, got %v", err)
	}

	bad := []AnteFailedCandidate{{Height: mainnetfx.HeightEx1Failed, TxIndex: 999}}
	if _, err := VerifyAnteFailedCandidates(context.Background(), newFixtureFetcher(t), bad, 1, nil); err == nil {
		t.Fatalf("expected missing tx result error")
	}

	nilEntry := &fixtureFetcher{errAt: map[int64]error{}, results: map[int64]*coretypes.ResultBlockResults{
		1: {Height: 1, TxResults: []*abci.ExecTxResult{nil}},
		2: nil,
	}}
	if _, err := VerifyAnteFailedCandidates(context.Background(), nilEntry, []AnteFailedCandidate{{Height: 1}}, 1, nil); err == nil {
		t.Fatalf("expected nil tx result error")
	}
	if _, err := VerifyAnteFailedCandidates(context.Background(), nilEntry, []AnteFailedCandidate{{Height: 2}}, 1, nil); err == nil {
		t.Fatalf("expected nil block results error")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyAnteFailedCandidates(cancelled, newFixtureFetcher(t), candidates, 2, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}

	empty, err := VerifyAnteFailedCandidates(context.Background(), newFixtureFetcher(t), nil, 4, nil)
	if err != nil || empty.CheckedHeights != 0 || len(empty.Confirmed) != 0 || len(empty.Heights()) != 0 {
		t.Fatalf("unexpected empty verification %+v %v", empty, err)
	}
}

// slowFetcher blocks until the context is cancelled, so cancellation happens
// while heights are still being fed.
type slowFetcher struct{}

func (slowFetcher) BlockResults(ctx context.Context, _ *int64) (*coretypes.ResultBlockResults, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestVerifyAnteFailedCandidatesCancelledWhileFeeding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	candidates := []AnteFailedCandidate{{Height: 1}, {Height: 2}, {Height: 3}}
	if _, err := VerifyAnteFailedCandidates(ctx, slowFetcher{}, candidates, 1, nil); err == nil {
		t.Fatalf("expected an error after cancellation")
	}
}

func TestPlanAnteFailedRepair(t *testing.T) {
	empty := PlanAnteFailedRepair(nil, nil)
	if empty.VerifiedHeights == nil || empty.ConflictHeights == nil || empty.Heights == nil || empty.Ranges == nil {
		t.Fatalf("empty plan must use non-nil slices: %+v", empty)
	}
	if len(empty.Heights) != 0 || len(empty.Ranges) != 0 {
		t.Fatalf("unexpected empty plan %+v", empty)
	}

	scan := &AnteFailedScanResult{Conflicts: []TxHashConflict{
		{Height: 6, OwnerHeight: 5},
		{Height: 6, OwnerHeight: 0},
		{Height: 20, OwnerHeight: 8},
	}}
	verification := &AnteFailedVerification{Confirmed: []VerifiedCandidate{
		{AnteFailedCandidate: AnteFailedCandidate{Height: 8}},
		{AnteFailedCandidate: AnteFailedCandidate{Height: 5}},
		{AnteFailedCandidate: AnteFailedCandidate{Height: 5}},
	}}
	plan := PlanAnteFailedRepair(scan, verification)
	assertHeights(t, "verified", plan.VerifiedHeights, []int64{5, 8})
	assertHeights(t, "conflict", plan.ConflictHeights, []int64{5, 6, 8, 20})
	assertHeights(t, "heights", plan.Heights, []int64{5, 6, 8, 20})
	wantRanges := []BlockRange{{Start: 5, End: 6}, {Start: 8, End: 8}, {Start: 20, End: 20}}
	if len(plan.Ranges) != len(wantRanges) {
		t.Fatalf("ranges %+v want %+v", plan.Ranges, wantRanges)
	}
	for i := range wantRanges {
		if plan.Ranges[i] != wantRanges[i] {
			t.Fatalf("ranges %+v want %+v", plan.Ranges, wantRanges)
		}
	}
}

func TestMigrationMarkerRoundTrip(t *testing.T) {
	db := dbm.NewMemDB()
	marker, err := LoadMigrationMarker(db, AnteFailedTxsMigration)
	if err != nil || marker != nil {
		t.Fatalf("missing marker: %+v %v", marker, err)
	}

	want := MigrationMarker{Name: AnteFailedTxsMigration, CompletedAt: time.Unix(1700000000, 0).UTC(), ResyncedCount: 4, Version: "v1"}
	if err := SaveMigrationMarker(db, want); err != nil {
		t.Fatalf("SaveMigrationMarker: %v", err)
	}
	if key := MigrationKey(AnteFailedTxsMigration); key[0] != KeyPrefixMigration || string(key[1:]) != AnteFailedTxsMigration {
		t.Fatalf("unexpected migration key %x", key)
	}
	got, err := LoadMigrationMarker(db, AnteFailedTxsMigration)
	if err != nil || got == nil || !got.CompletedAt.Equal(want.CompletedAt) || got.Name != want.Name || got.ResyncedCount != 4 || got.Version != "v1" {
		t.Fatalf("round trip: %+v %v", got, err)
	}

	if err := db.Set(MigrationKey("broken"), []byte("{")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := LoadMigrationMarker(db, "broken"); err == nil {
		t.Fatalf("expected decode error")
	}
	if _, err := LoadMigrationMarker(failingGetDB{DB: db, prefix: KeyPrefixMigration}, AnteFailedTxsMigration); err == nil {
		t.Fatalf("expected get error")
	}
	if err := SaveMigrationMarker(failingSetDB{DB: db}, want); err == nil {
		t.Fatalf("expected save error")
	}
}

type failingSetDB struct {
	dbm.DB
}

func (failingSetDB) SetSync([]byte, []byte) error { return errors.New("injected set failure") }

func TestMigrationHelpers(t *testing.T) {
	assertHeights(t, "unique empty", uniqueSortedHeights(nil), []int64{})
	assertHeights(t, "unique", uniqueSortedHeights([]int64{3, 1, 3, 2, 1}), []int64{1, 2, 3})

	if got := truncateLog("short", 10); got != "short" {
		t.Fatalf("truncateLog short: %q", got)
	}
	if got := truncateLog("0123456789", 4); got != "0123..." {
		t.Fatalf("truncateLog long: %q", got)
	}

	for _, tc := range []struct {
		opts   AnteFailedScanOptions
		height int64
		want   bool
	}{
		{AnteFailedScanOptions{}, 1, true},
		{AnteFailedScanOptions{From: 5}, 4, false},
		{AnteFailedScanOptions{From: 5}, 5, true},
		{AnteFailedScanOptions{To: 5}, 6, false},
		{AnteFailedScanOptions{To: 5}, 5, true},
	} {
		if got := tc.opts.contains(tc.height); got != tc.want {
			t.Fatalf("%+v contains %d = %v", tc.opts, tc.height, got)
		}
	}

	scan := &AnteFailedScanResult{Candidates: []AnteFailedCandidate{{Height: 9}, {Height: 2}, {Height: 9}}}
	assertHeights(t, "candidate heights", scan.CandidateHeights(), []int64{2, 9})
}

// TestAnteFailedMigrationFlowRepairsLegacyState runs scan, verify, plan and
// apply against the pre-fix KV state and expects the state a fresh index of
// the same blocks produces.
func TestAnteFailedMigrationFlowRepairsLegacyState(t *testing.T) {
	db := loadLegacyState(t)
	scan := scanLegacy(t, db, AnteFailedScanOptions{})
	verification, err := VerifyAnteFailedCandidates(context.Background(), newFixtureFetcher(t), scan.Candidates, 2, testLogger())
	if err != nil {
		t.Fatalf("VerifyAnteFailedCandidates: %v", err)
	}
	plan := PlanAnteFailedRepair(scan, verification)
	assertHeights(t, "plan heights", plan.Heights, mainnetfx.Heights)

	kv := newFixtureIndexer(t, db, WithCachedBlockGasLimit(legacyGasLimit(t)))
	for _, blockRange := range plan.Ranges {
		for height := blockRange.Start; height <= blockRange.End; height++ {
			indexFixture(t, kv, height)
		}
	}
	if err := SaveMigrationMarker(db, MigrationMarker{Name: AnteFailedTxsMigration, ResyncedCount: len(plan.Heights)}); err != nil {
		t.Fatalf("SaveMigrationMarker: %v", err)
	}

	assertSameDB(t, dumpDB(t, db, KeyPrefixMigration), dumpDB(t, freshFixtureDB(t, mainnetfx.Heights...)))

	// a second scan finds nothing left to repair
	rescan := scanLegacy(t, db, AnteFailedScanOptions{})
	if len(rescan.Candidates) != 0 || len(rescan.Conflicts) != 0 {
		t.Fatalf("expected a clean state after repair: %+v", rescan)
	}
}

func assertHeights(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: nil heights", label)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %v want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: %v want %v", label, got, want)
		}
	}
}
