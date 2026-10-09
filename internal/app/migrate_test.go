package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	dbm "github.com/cosmos/cosmos-db"
	"google.golang.org/grpc"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func migrateTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// migrateTestConfig returns a config whose indexer DB lives in a temp dir.
func migrateTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBBackend = string(dbm.GoLevelDBBackend)
	cfg.FetchJobs = 2
	return cfg
}

// seedLegacyState writes the KV state left by the pre-fix indexer.
func seedLegacyState(t *testing.T, cfg config.Config) {
	t.Helper()
	db, err := openIndexerDB(cfg.DataDir, cfg.DBBackend)
	if err != nil {
		t.Fatalf("openIndexerDB: %v", err)
	}
	defer db.Close()
	kvs, err := mainnetfx.LegacyReporterState()
	if err != nil {
		t.Fatalf("LegacyReporterState: %v", err)
	}
	for _, kv := range kvs {
		if err := db.Set(kv.Key, kv.Value); err != nil {
			t.Fatalf("seed legacy state: %v", err)
		}
	}
}

type kvSnapshot map[string][]byte

func snapshotDB(t *testing.T, db dbm.DB, skipPrefix byte) kvSnapshot {
	t.Helper()
	it, err := db.Iterator(nil, nil)
	if err != nil {
		t.Fatalf("iterator: %v", err)
	}
	defer it.Close()
	out := make(kvSnapshot)
	for ; it.Valid(); it.Next() {
		if len(it.Key()) > 0 && it.Key()[0] == skipPrefix {
			continue
		}
		out[string(it.Key())] = append([]byte(nil), it.Value()...)
	}
	return out
}

func snapshotDataDir(t *testing.T, cfg config.Config, skipPrefix byte) kvSnapshot {
	t.Helper()
	db, err := openIndexerDB(cfg.DataDir, cfg.DBBackend)
	if err != nil {
		t.Fatalf("openIndexerDB: %v", err)
	}
	defer db.Close()
	return snapshotDB(t, db, skipPrefix)
}

func assertSnapshotsEqual(t *testing.T, got, want kvSnapshot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("snapshot size mismatch: got %d keys want %d", len(got), len(want))
	}
	for key, value := range want {
		gotValue, ok := got[key]
		if !ok {
			t.Fatalf("missing key %x", key)
		}
		if !bytes.Equal(gotValue, value) {
			t.Fatalf("value mismatch for key %x", key)
		}
	}
}

func newFixtureIndexer(t *testing.T, db dbm.DB) *txindexer.KVIndexer {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	return txindexer.NewKVIndexer(db, migrateTestLogger(), clientCtx)
}

func indexFixtureHeight(t *testing.T, kv *txindexer.KVIndexer, height int64) txindexer.BlockIndexStats {
	t.Helper()
	block, err := mainnetfx.Block(height)
	if err != nil {
		t.Fatalf("Block(%d): %v", height, err)
	}
	results, err := mainnetfx.BlockResults(height)
	if err != nil {
		t.Fatalf("BlockResults(%d): %v", height, err)
	}
	stats, err := kv.IndexBlockWithStatsAndResults(block, results)
	if err != nil {
		t.Fatalf("index %d: %v", height, err)
	}
	return stats
}

// freshFixtureState indexes every fixture height with the current indexer.
func freshFixtureState(t *testing.T) kvSnapshot {
	t.Helper()
	db := dbm.NewMemDB()
	kv := newFixtureIndexer(t, db)
	for _, height := range mainnetfx.Heights {
		indexFixtureHeight(t, kv, height)
	}
	return snapshotDB(t, db, txindexer.KeyPrefixMigration)
}

type fixtureFetcher struct {
	mu      sync.Mutex
	results mainnetfx.BlockResultsMap
	err     error
	calls   int
}

func (f *fixtureFetcher) BlockResults(_ context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.results[*height], nil
}

// fakeNode serves fixtures and resyncs through a real indexer on the DB the
// migration opened.
type fakeNode struct {
	t          *testing.T
	fetcher    *fixtureFetcher
	openErr    error
	resyncErr  error
	opened     int
	closed     int
	resyncedAt []txindexer.BlockRange
}

func (n *fakeNode) deps() anteFailedMigrationDeps {
	return anteFailedMigrationDeps{
		openNode: func(_ context.Context, _ config.Config, _ string, db dbm.DB, _ *slog.Logger) (txindexer.BlockResultsFetcher, func(context.Context, []txindexer.BlockRange) (txindexer.ResyncStats, error), func(), error) {
			n.opened++
			if n.openErr != nil {
				return nil, nil, nil, n.openErr
			}
			kv := newFixtureIndexer(n.t, db)
			resync := func(_ context.Context, ranges []txindexer.BlockRange) (txindexer.ResyncStats, error) {
				n.resyncedAt = append(n.resyncedAt, ranges...)
				if n.resyncErr != nil {
					return txindexer.ResyncStats{}, n.resyncErr
				}
				var stats txindexer.ResyncStats
				for _, r := range ranges {
					for height := r.Start; height <= r.End; height++ {
						blockStats := indexFixtureHeight(n.t, kv, height)
						stats.BlocksSynced++
						stats.UniqueTxnsSeen += blockStats.IndexedEthTxs
					}
				}
				return stats, nil
			}
			return n.fetcher, resync, func() { n.closed++ }, nil
		},
	}
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	results, err := mainnetfx.AllBlockResults()
	if err != nil {
		t.Fatalf("AllBlockResults: %v", err)
	}
	return &fakeNode{t: t, fetcher: &fixtureFetcher{results: results}}
}

func readMigrationReport(t *testing.T, path string) AnteFailedMigrationReport {
	t.Helper()
	bz, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report AnteFailedMigrationReport
	if err := json.Unmarshal(bz, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return report
}

func loadMarker(t *testing.T, cfg config.Config) *txindexer.MigrationMarker {
	t.Helper()
	db, err := openIndexerDB(cfg.DataDir, cfg.DBBackend)
	if err != nil {
		t.Fatalf("openIndexerDB: %v", err)
	}
	defer db.Close()
	marker, err := txindexer.LoadMigrationMarker(db, txindexer.AnteFailedTxsMigration)
	if err != nil {
		t.Fatalf("LoadMigrationMarker: %v", err)
	}
	return marker
}

var expectedRepairHeights = []int64{
	mainnetfx.HeightEx1Failed,
	mainnetfx.HeightEx1Included,
	mainnetfx.HeightEx2Failed,
	mainnetfx.HeightEx2Included,
}

func assertHeights(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
}

func TestMigrateAnteFailedScanOnlyIsOfflineAndReadOnly(t *testing.T) {
	cfg := migrateTestConfig(t)
	seedLegacyState(t, cfg)
	before := snapshotDataDir(t, cfg, 0xff)
	node := newFakeNode(t)
	reportPath := filepath.Join(t.TempDir(), "nested", "scan.json")

	err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ScanOnly: true, ReportPath: reportPath}, node.deps())
	if err != nil {
		t.Fatalf("scan-only migration: %v", err)
	}
	if node.opened != 0 {
		t.Fatalf("scan-only must not open the node, opened %d times", node.opened)
	}
	assertSnapshotsEqual(t, snapshotDataDir(t, cfg, 0xff), before)

	report := readMigrationReport(t, reportPath)
	if report.Mode != "scan-only" || report.Migration != txindexer.AnteFailedTxsMigration {
		t.Fatalf("unexpected report header: %+v", report)
	}
	if report.Scan == nil || len(report.Scan.Candidates) != 10 || len(report.Scan.Conflicts) != 5 {
		t.Fatalf("unexpected scan report: %+v", report.Scan)
	}
	if report.Verification != nil || report.Resync != nil {
		t.Fatalf("scan-only report must not contain verification or resync")
	}
	if report.Plan == nil || len(report.Plan.VerifiedHeights) != 0 {
		t.Fatalf("unexpected scan-only plan: %+v", report.Plan)
	}
	assertHeights(t, "scan-only plan", report.Plan.Heights, expectedRepairHeights)
	if report.FinishedAt.Before(report.StartedAt) {
		t.Fatalf("report finished before it started")
	}
}

func TestMigrateAnteFailedDryRunVerifiesWithoutWriting(t *testing.T) {
	cfg := migrateTestConfig(t)
	seedLegacyState(t, cfg)
	before := snapshotDataDir(t, cfg, 0xff)
	node := newFakeNode(t)
	reportPath := filepath.Join(t.TempDir(), "dry.json")

	err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{DryRun: true, ReportPath: reportPath}, node.deps())
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if node.opened != 1 || node.closed != 1 {
		t.Fatalf("dry-run must open and close the node once: opened %d closed %d", node.opened, node.closed)
	}
	if node.fetcher.calls != 2 {
		t.Fatalf("dry-run must fetch block results only for the 2 candidate heights, fetched %d", node.fetcher.calls)
	}
	if len(node.resyncedAt) != 0 {
		t.Fatalf("dry-run must not resync, resynced %v", node.resyncedAt)
	}
	assertSnapshotsEqual(t, snapshotDataDir(t, cfg, 0xff), before)
	if marker := loadMarker(t, cfg); marker != nil {
		t.Fatalf("dry-run must not write a marker: %+v", marker)
	}

	report := readMigrationReport(t, reportPath)
	if report.Mode != "dry-run" || report.Verification == nil || len(report.Verification.Confirmed) != 10 {
		t.Fatalf("unexpected dry-run report: %+v", report.Verification)
	}
	for _, confirmed := range report.Verification.Confirmed {
		if confirmed.Verdict != txindexer.VerdictAnteFailed || confirmed.Code != 5 || confirmed.Codespace != "sdk" {
			t.Fatalf("unexpected confirmed candidate: %+v", confirmed)
		}
	}
	assertHeights(t, "dry-run verified heights", report.Plan.VerifiedHeights, []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx2Failed})
	assertHeights(t, "dry-run plan", report.Plan.Heights, expectedRepairHeights)
}

func TestMigrateAnteFailedApplyRepairsLegacyState(t *testing.T) {
	cfg := migrateTestConfig(t)
	seedLegacyState(t, cfg)
	node := newFakeNode(t)
	reportPath := filepath.Join(t.TempDir(), "apply.json")

	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ReportPath: reportPath}, node.deps()); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if len(node.resyncedAt) != len(expectedRepairHeights) {
		t.Fatalf("unexpected resync ranges: %v", node.resyncedAt)
	}
	for i, r := range node.resyncedAt {
		if r.Start != expectedRepairHeights[i] || r.End != expectedRepairHeights[i] {
			t.Fatalf("unexpected resync range %d: %+v", i, r)
		}
	}

	// The repaired state must be exactly what the fixed indexer writes from
	// scratch, whatever order the legacy state was written in.
	assertSnapshotsEqual(t, snapshotDataDir(t, cfg, txindexer.KeyPrefixMigration), freshFixtureState(t))

	marker := loadMarker(t, cfg)
	if marker == nil || marker.Name != txindexer.AnteFailedTxsMigration || marker.ResyncedCount != len(expectedRepairHeights) || marker.CompletedAt.IsZero() {
		t.Fatalf("unexpected marker: %+v", marker)
	}
	report := readMigrationReport(t, reportPath)
	if report.Mode != "apply" || report.Resync == nil || report.Resync.BlocksSynced != int64(len(expectedRepairHeights)) {
		t.Fatalf("unexpected apply report: %+v", report.Resync)
	}

	// A second run is a no-op guarded by the marker.
	rerun := newFakeNode(t)
	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, rerun.deps()); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	if rerun.opened != 0 {
		t.Fatalf("completed migration must not run again without --force")
	}

	// --force runs again and finds nothing left to repair: the remaining
	// candidates (post-ante failures storing the gas limit) are verified and
	// rejected.
	forced := newFakeNode(t)
	forcedReport := filepath.Join(t.TempDir(), "forced.json")
	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{Force: true, ReportPath: forcedReport}, forced.deps()); err != nil {
		t.Fatalf("forced migration: %v", err)
	}
	if forced.opened != 1 || forced.fetcher.calls != 1 || len(forced.resyncedAt) != 0 {
		t.Fatalf("forced rerun on repaired state: opened %d fetched %d resynced %v", forced.opened, forced.fetcher.calls, forced.resyncedAt)
	}
	report = readMigrationReport(t, forcedReport)
	if len(report.Scan.Conflicts) != 0 || len(report.Verification.Confirmed) != 0 || len(report.Plan.Heights) != 0 || report.Resync != nil {
		t.Fatalf("repaired state must have nothing to repair: %+v", report.Plan)
	}
	if marker := loadMarker(t, cfg); marker == nil || marker.ResyncedCount != 0 {
		t.Fatalf("forced rerun must refresh the marker: %+v", marker)
	}

	// Scan-only and dry-run still work after completion without --force.
	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ScanOnly: true}, newFakeNode(t).deps()); err != nil {
		t.Fatalf("scan-only after completion: %v", err)
	}
	dry := newFakeNode(t)
	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{DryRun: true}, dry.deps()); err != nil {
		t.Fatalf("dry-run after completion: %v", err)
	}
	if dry.opened != 1 {
		t.Fatalf("dry-run after completion must run")
	}
}

func TestMigrateAnteFailedHeightRangeLimitsScan(t *testing.T) {
	cfg := migrateTestConfig(t)
	seedLegacyState(t, cfg)
	node := newFakeNode(t)
	reportPath := filepath.Join(t.TempDir(), "range.json")

	err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{
		From:       mainnetfx.HeightEx2Failed,
		To:         mainnetfx.HeightEx2Included,
		DryRun:     true,
		ReportPath: reportPath,
	}, node.deps())
	if err != nil {
		t.Fatalf("ranged dry-run: %v", err)
	}
	report := readMigrationReport(t, reportPath)
	if len(report.Scan.Candidates) != 8 || len(report.Scan.Conflicts) != 4 {
		t.Fatalf("unexpected ranged scan: %d candidates %d conflicts", len(report.Scan.Candidates), len(report.Scan.Conflicts))
	}
	assertHeights(t, "ranged plan", report.Plan.Heights, []int64{mainnetfx.HeightEx2Failed, mainnetfx.HeightEx2Included})
}

func TestMigrateAnteFailedErrors(t *testing.T) {
	t.Run("invalid range", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{From: 10, To: 5}, newFakeNode(t).deps())
		if err == nil || !strings.Contains(err.Error(), "invalid height range") {
			t.Fatalf("expected invalid range error, got %v", err)
		}
	})

	t.Run("public entrypoint validates range", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		err := RunMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{From: 10, To: 5})
		if err == nil || !strings.Contains(err.Error(), "invalid height range") {
			t.Fatalf("expected invalid range error, got %v", err)
		}
	})

	t.Run("data dir is a file", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		cfg.DataDir = filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(cfg.DataDir, nil, 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, newFakeNode(t).deps()); err == nil {
			t.Fatalf("expected data dir error")
		}
	})

	t.Run("bad db backend", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		cfg.DBBackend = "nosuchbackend"
		if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, newFakeNode(t).deps()); err == nil {
			t.Fatalf("expected db backend error")
		}
	})

	t.Run("corrupt marker", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		db, err := openIndexerDB(cfg.DataDir, cfg.DBBackend)
		if err != nil {
			t.Fatalf("openIndexerDB: %v", err)
		}
		if err := db.Set(txindexer.MigrationKey(txindexer.AnteFailedTxsMigration), []byte("not json")); err != nil {
			t.Fatalf("set marker: %v", err)
		}
		db.Close()
		if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, newFakeNode(t).deps()); err == nil {
			t.Fatalf("expected corrupt marker error")
		}
	})

	t.Run("open node", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		seedLegacyState(t, cfg)
		node := newFakeNode(t)
		node.openErr = errors.New("node down")
		err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, node.deps())
		if err == nil || !strings.Contains(err.Error(), "node down") {
			t.Fatalf("expected open node error, got %v", err)
		}
	})

	t.Run("verify", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		seedLegacyState(t, cfg)
		node := newFakeNode(t)
		node.fetcher.err = errors.New("archive unavailable")
		err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, node.deps())
		if err == nil || !strings.Contains(err.Error(), "verify candidates") || !strings.Contains(err.Error(), "archive unavailable") {
			t.Fatalf("expected verify error, got %v", err)
		}
		if node.closed != 1 {
			t.Fatalf("node must be closed on error")
		}
	})

	t.Run("resync keeps report and skips marker", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		seedLegacyState(t, cfg)
		node := newFakeNode(t)
		node.resyncErr = errors.New("resync broke")
		reportPath := filepath.Join(t.TempDir(), "failed.json")
		err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ReportPath: reportPath}, node.deps())
		if err == nil || !strings.Contains(err.Error(), "resync affected heights") {
			t.Fatalf("expected resync error, got %v", err)
		}
		if marker := loadMarker(t, cfg); marker != nil {
			t.Fatalf("failed migration must not write a marker: %+v", marker)
		}
		report := readMigrationReport(t, reportPath)
		if report.Plan == nil || len(report.Plan.Heights) != len(expectedRepairHeights) || report.Resync != nil {
			t.Fatalf("unexpected failed report: %+v", report)
		}
	})

	t.Run("scan cancelled", func(t *testing.T) {
		cfg := migrateTestConfig(t)
		seedLegacyState(t, cfg)
		node := newFakeNode(t)
		// A blocked report path makes report writing fail after the scan.
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatalf("write blocker: %v", err)
		}
		err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ScanOnly: true, ReportPath: filepath.Join(blocker, "sub", "r.json")}, node.deps())
		if err == nil || !strings.Contains(err.Error(), "migration report") {
			t.Fatalf("expected report dir error, got %v", err)
		}
		err = runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{ScanOnly: true, ReportPath: t.TempDir()}, node.deps())
		if err == nil || !strings.Contains(err.Error(), "write migration report") {
			t.Fatalf("expected report write error, got %v", err)
		}
	})
}

func TestMigrateAnteFailedEmptyStateMarksCompletion(t *testing.T) {
	cfg := migrateTestConfig(t)
	node := newFakeNode(t)
	if err := runMigrateAnteFailed(cfg, migrateTestLogger(), MigrateAnteFailedOptions{}, node.deps()); err != nil {
		t.Fatalf("migration on empty state: %v", err)
	}
	if node.fetcher.calls != 0 || len(node.resyncedAt) != 0 {
		t.Fatalf("empty state must not fetch or resync")
	}
	if marker := loadMarker(t, cfg); marker == nil || marker.ResyncedCount != 0 {
		t.Fatalf("expected completion marker, got %+v", marker)
	}
}

func TestMigrationMode(t *testing.T) {
	cases := map[string]MigrateAnteFailedOptions{
		"scan-only": {ScanOnly: true, DryRun: true},
		"dry-run":   {DryRun: true},
		"apply":     {},
	}
	for want, opts := range cases {
		if got := migrationMode(opts); got != want {
			t.Fatalf("migrationMode(%+v) = %q want %q", opts, got, want)
		}
	}
}

func TestOpenMigrationNodeRequiresCometClient(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnableSync = false
	cfg.OfflineRPCOnly = true
	cfg.ChainID = "injective-1"
	cfg.EVMChainID = "1776"

	_, _, _, err := openMigrationNode(context.Background(), cfg, t.TempDir(), dbm.NewMemDB(), migrateTestLogger())
	if err == nil || !strings.Contains(err.Error(), "comet rpc client unavailable") {
		t.Fatalf("expected missing comet client error, got %v", err)
	}
}

// TestOpenMigrationNodeOnline wires the migration node against a stub Comet
// RPC server and a gRPC server without services (EVM params unimplemented,
// accepted for virtualized indexing with a configured EVM chain id).
func TestOpenMigrationNodeOnline(t *testing.T) {
	comet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "status":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"node_info":{"network":"injective-1","protocol_version":{"p2p":"0","block":"0","app":"0"}},"sync_info":{"latest_block_height":"0"},"validator_info":{"voting_power":"0"}}}`, req.ID)
		case "block_results":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"height":"7"}}`, req.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)
		}
	}))
	defer comet.Close()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	cfg := config.DefaultConfig()
	cfg.ChainID = "injective-1"
	cfg.EVMChainID = "1776"
	cfg.VirtualizeCosmosEvents = true
	cfg.CometRPC = comet.URL
	cfg.GRPCAddr = lis.Addr().String()

	fetcher, resync, cleanup, err := openMigrationNode(context.Background(), cfg, t.TempDir(), dbm.NewMemDB(), migrateTestLogger())
	if err != nil {
		t.Fatalf("openMigrationNode: %v", err)
	}
	defer cleanup()
	if fetcher == nil || resync == nil {
		t.Fatalf("expected fetcher and resync function")
	}
	height := int64(7)
	res, err := fetcher.BlockResults(context.Background(), &height)
	if err != nil || res == nil || res.Height != 7 {
		t.Fatalf("BlockResults through migration node: %+v %v", res, err)
	}
	// The resync function fetches scattered heights through the syncer.
	if _, err := resync(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no resync heights") {
		t.Fatalf("expected empty heights error from resync, got %v", err)
	}
}

func TestExpandRanges(t *testing.T) {
	got := expandRanges([]txindexer.BlockRange{{Start: 3, End: 5}, {Start: 9, End: 9}})
	want := []int64{3, 4, 5, 9}
	if len(got) != len(want) {
		t.Fatalf("expandRanges = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expandRanges = %v want %v", got, want)
		}
	}
	if got := expandRanges(nil); len(got) != 0 {
		t.Fatalf("expandRanges(nil) = %v", got)
	}
}

func TestOpenMigrationNodeUnreachableComet(t *testing.T) {
	comet := httptest.NewServer(http.NotFoundHandler())
	url := comet.URL
	comet.Close()

	cfg := config.DefaultConfig()
	cfg.ChainID = "injective-1"
	cfg.EVMChainID = "1776"
	cfg.CometRPC = url

	if _, _, _, err := openMigrationNode(context.Background(), cfg, t.TempDir(), dbm.NewMemDB(), migrateTestLogger()); err == nil {
		t.Fatalf("expected unreachable comet error")
	}
}
