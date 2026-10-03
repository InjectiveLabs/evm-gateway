package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/pkg/errors"
	"upd.dev/xlab/gotracer"

	"github.com/InjectiveLabs/evm-gateway/internal/config"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/version"
)

// MigrateAnteFailedOptions controls the ante-failed txs migration.
type MigrateAnteFailedOptions struct {
	// From and To bound the scanned heights (inclusive, 0 = open).
	From int64
	To   int64
	// ScanOnly stops after the offline scan of local state; no network access.
	ScanOnly bool
	// DryRun verifies candidates against the node but writes nothing.
	DryRun bool
	// Force reruns the migration even if its completion marker exists.
	Force bool
	// ReportPath, when set, receives a JSON report of every phase.
	ReportPath string
}

// AnteFailedMigrationReport is the JSON report written by the migration.
type AnteFailedMigrationReport struct {
	Migration    string                            `json:"migration"`
	Version      string                            `json:"version"`
	StartedAt    time.Time                         `json:"started_at"`
	FinishedAt   time.Time                         `json:"finished_at"`
	Mode         string                            `json:"mode"`
	Scan         *txindexer.AnteFailedScanResult   `json:"scan,omitempty"`
	Verification *txindexer.AnteFailedVerification `json:"verification,omitempty"`
	Plan         *txindexer.AnteFailedRepairPlan   `json:"plan,omitempty"`
	Resync       *txindexer.ResyncStats            `json:"resync,omitempty"`
}

// anteFailedMigrationDeps abstracts the network-facing parts of the migration
// so that every phase can be exercised with static fixtures.
type anteFailedMigrationDeps struct {
	// openNode returns a block results fetcher and a resync function backed by
	// the configured (archival) node, plus a cleanup function.
	openNode func(ctx context.Context, cfg config.Config, dataDir string, db dbm.DB, logger *slog.Logger) (txindexer.BlockResultsFetcher, func(context.Context, []txindexer.BlockRange) (txindexer.ResyncStats, error), func(), error)
}

// RunMigrateAnteFailed repairs indexed state written by indexer versions that
// exposed Ethereum txs from Cosmos txs that failed in the ante handler:
//  1. scan local KV state offline for candidate txs and hash ownership
//     conflicts;
//  2. fetch block results only for candidate heights to confirm them;
//  3. resync only the confirmed and conflicting heights.
func RunMigrateAnteFailed(cfg config.Config, logger *slog.Logger, opts MigrateAnteFailedOptions) error {
	return runMigrateAnteFailed(cfg, logger, opts, anteFailedMigrationDeps{openNode: openMigrationNode})
}

func runMigrateAnteFailed(cfg config.Config, logger *slog.Logger, opts MigrateAnteFailedOptions, deps anteFailedMigrationDeps) error {
	cfg.Normalize()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer gotracer.Trace(&ctx, appTraceTag)()

	if opts.From > 0 && opts.To > 0 && opts.From > opts.To {
		return errors.Errorf("invalid height range: from %d is greater than to %d", opts.From, opts.To)
	}

	dataDir, err := expandHome(cfg.DataDir)
	if err != nil {
		return err
	}
	idxDB, err := openIndexerDB(dataDir, cfg.DBBackend)
	if err != nil {
		return err
	}
	defer func() {
		_ = idxDB.Close()
	}()

	report := &AnteFailedMigrationReport{
		Migration: txindexer.AnteFailedTxsMigration,
		Version:   version.AppVersion,
		StartedAt: time.Now().UTC(),
		Mode:      migrationMode(opts),
	}

	marker, err := txindexer.LoadMigrationMarker(idxDB, txindexer.AnteFailedTxsMigration)
	if err != nil {
		return err
	}
	if marker != nil && !opts.Force && !opts.ScanOnly && !opts.DryRun {
		logger.Info(
			"migration already completed; use --force to run it again",
			"migration", marker.Name,
			"completed_at", marker.CompletedAt,
			"resynced_heights", marker.ResyncedCount,
		)
		return nil
	}

	baseCtx, err := baseClientContext(ctx, dataDir)
	if err != nil {
		return err
	}

	logger.Info("phase 1/3: scanning local state", "from", opts.From, "to", opts.To)
	scan, err := txindexer.ScanAnteFailedCandidates(ctx, idxDB, baseCtx.Codec, txindexer.AnteFailedScanOptions{From: opts.From, To: opts.To}, logger)
	if err != nil {
		return errors.Wrap(err, "scan local state")
	}
	report.Scan = scan
	logger.Info(
		"local scan complete",
		"scanned_txs", scan.ScannedTxs,
		"failed_txs", scan.FailedTxs,
		"candidates", len(scan.Candidates),
		"candidate_heights", len(scan.CandidateHeights()),
		"conflicts", len(scan.Conflicts),
		"conflict_heights", len(scan.ConflictHeights()),
	)
	if opts.ScanOnly {
		plan := txindexer.PlanAnteFailedRepair(scan, nil)
		report.Plan = &plan
		return finishMigrationReport(report, opts.ReportPath, logger)
	}

	fetcher, resync, closeNode, err := deps.openNode(ctx, cfg, dataDir, idxDB, logger)
	if err != nil {
		return err
	}
	defer closeNode()

	logger.Info("phase 2/3: verifying candidates against block results", "heights", len(scan.CandidateHeights()), "jobs", cfg.FetchJobs)
	verification, err := txindexer.VerifyAnteFailedCandidates(ctx, fetcher, scan.Candidates, cfg.FetchJobs, logger)
	if err != nil {
		return errors.Wrap(err, "verify candidates")
	}
	report.Verification = verification
	plan := txindexer.PlanAnteFailedRepair(scan, verification)
	report.Plan = &plan
	logger.Info(
		"verification complete",
		"checked_heights", verification.CheckedHeights,
		"confirmed_txs", len(verification.Confirmed),
		"verified_heights", len(plan.VerifiedHeights),
		"conflict_heights", len(plan.ConflictHeights),
		"heights_to_resync", len(plan.Heights),
	)
	if opts.DryRun {
		return finishMigrationReport(report, opts.ReportPath, logger)
	}

	if len(plan.Ranges) > 0 {
		logger.Info("phase 3/3: resyncing affected heights", "heights", len(plan.Heights), "segments", len(plan.Ranges))
		stats, err := resync(ctx, plan.Ranges)
		if err != nil {
			_ = finishMigrationReport(report, opts.ReportPath, logger)
			return errors.Wrap(err, "resync affected heights")
		}
		report.Resync = &stats
		logger.Info("resync complete", "blocks_synced", stats.BlocksSynced, "unique_txns_seen", stats.UniqueTxnsSeen)
	} else {
		logger.Info("phase 3/3: nothing to resync")
	}

	if err := txindexer.SaveMigrationMarker(idxDB, txindexer.MigrationMarker{
		Name:          txindexer.AnteFailedTxsMigration,
		CompletedAt:   time.Now().UTC(),
		ResyncedCount: len(plan.Heights),
		Version:       version.AppVersion,
	}); err != nil {
		return err
	}
	return finishMigrationReport(report, opts.ReportPath, logger)
}

func migrationMode(opts MigrateAnteFailedOptions) string {
	switch {
	case opts.ScanOnly:
		return "scan-only"
	case opts.DryRun:
		return "dry-run"
	default:
		return "apply"
	}
}

func finishMigrationReport(report *AnteFailedMigrationReport, path string, logger *slog.Logger) error {
	report.FinishedAt = time.Now().UTC()
	if path == "" {
		return nil
	}
	bz, err := sonic.ConfigStd.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.Wrap(err, "encode migration report")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return errors.Wrap(err, "create migration report dir")
		}
	}
	if err := os.WriteFile(path, bz, 0o644); err != nil {
		return errors.Wrap(err, "write migration report")
	}
	logger.Info("migration report written", "path", path)
	return nil
}

// openMigrationNode wires the configured Comet/gRPC endpoints exactly like the
// resync command does.
func openMigrationNode(
	ctx context.Context,
	cfg config.Config,
	dataDir string,
	db dbm.DB,
	logger *slog.Logger,
) (txindexer.BlockResultsFetcher, func(context.Context, []txindexer.BlockRange) (txindexer.ResyncStats, error), func(), error) {
	clientCtx, rpcClient, grpcConn, err := buildClientContext(ctx, &cfg, dataDir, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() {
		if rpcClient != nil {
			_ = rpcClient.Stop()
		}
		if grpcConn != nil {
			_ = grpcConn.Close()
		}
	}
	if rpcClient == nil {
		cleanup()
		return nil, nil, nil, errors.New("comet rpc client unavailable")
	}

	txIndexer := txindexer.NewKVIndexer(db, logger.With("indexer", "evm"), clientCtx, buildKVIndexerOptions(ctx, cfg, clientCtx, logger)...)
	syncer := txindexer.NewSyncer(cfg, logger, rpcClient, db, txIndexer, nil)
	// Affected heights are scattered: fetch them concurrently instead of one
	// single-height range at a time.
	resync := func(ctx context.Context, ranges []txindexer.BlockRange) (txindexer.ResyncStats, error) {
		return syncer.ResyncHeights(ctx, expandRanges(ranges))
	}
	return rpcClient, resync, cleanup, nil
}

func expandRanges(ranges []txindexer.BlockRange) []int64 {
	heights := make([]int64, 0, txindexer.CountBlocks(ranges))
	for _, r := range ranges {
		for height := r.Start; height <= r.End; height++ {
			heights = append(heights, height)
		}
	}
	return heights
}
