package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"upd.dev/xlab/gotracer"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	pkgerrors "github.com/pkg/errors"

	"github.com/InjectiveLabs/evm-gateway/internal/blocksync"
	"github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
)

type syncClient interface {
	Status(context.Context) (*coretypes.ResultStatus, error)
	blocksync.BlockClient
}

// Syncer manages historical gap sync and forward indexing.
type Syncer struct {
	cfg     config.Config
	logger  *slog.Logger
	client  syncClient
	db      dbm.DB
	indexer TxIndexer
	status  *syncstatus.Tracker
}

type ResyncStats struct {
	BlocksSynced   int64
	UniqueTxnsSeen int64
}

type txIndexerWithStats interface {
	IndexBlockWithStats(block *cmtypes.Block, txResults []*abci.ExecTxResult) (BlockIndexStats, error)
}

const paceInterval = 10 * time.Second

var txIndexerSyncerTraceTag = gotracer.NewTag("component", "tx_indexer_syncer")

func NewSyncer(cfg config.Config, logger *slog.Logger, client syncClient, db dbm.DB, indexer TxIndexer, status *syncstatus.Tracker) *Syncer {
	return &Syncer{
		cfg:     cfg,
		logger:  logger.With("module", "indexer"),
		client:  client,
		db:      db,
		indexer: indexer,
		status:  status,
	}
}

// Run starts the indexer sync loop and blocks until context cancellation.
func (s *Syncer) Run(ctx context.Context) error {
	if s.indexer == nil {
		s.logger.Warn("tx indexer not configured")
		return nil
	}
	if !s.cfg.EnableSync {
		s.logger.Info("indexer sync disabled")
		if s.status != nil {
			s.status.SetPhase("sync_disabled")
		}
		return nil
	}
	if s.status != nil {
		s.status.SetPhase("initializing")
	}

	status, err := s.client.Status(ctx)
	if err != nil {
		return err
	}

	earliest := s.cfg.Earliest
	if earliest <= 0 {
		earliest = status.SyncInfo.EarliestBlockHeight
		if earliest <= 0 {
			earliest = 1
		}
	} else {
		earliest, err = s.resolveEarliestBlock(ctx, earliest)
		if err != nil {
			return err
		}
	}

	head := status.SyncInfo.LatestBlockHeight
	if s.status != nil {
		s.status.SetChainHead(head)
		s.status.SetEarliestBlock(earliest)
	}
	if head < earliest {
		s.logger.Info("chain head below earliest; nothing to sync", "head", head, "earliest", earliest)
		if s.status != nil {
			s.status.SetPhase("idle")
		}
		return nil
	}

	ranges, err := LoadIndexedRanges(s.db)
	if err != nil {
		return err
	}
	if len(ranges) == 0 {
		s.logger.Info("indexer db empty")
	} else {
		s.logger.Info("indexed ranges loaded", "count", len(ranges), "first", ranges[0], "last", ranges[len(ranges)-1])
	}

	gaps := ComputeGaps(earliest, head, ranges)
	if s.status != nil {
		s.status.SetPhase("initial_gap_sync")
		s.status.SetGaps(toStatusRanges(gaps))
	}
	if len(gaps) == 0 {
		s.logger.Info("no gaps detected; entering forward sync", "head", head)
	} else {
		for _, gap := range gaps {
			s.logger.Info("gap detected", "start", gap.Start, "end", gap.End)
		}
	}

	rangeSyncer := blocksync.NewSyncer(s.client, s.logger, s.cfg.FetchJobs, s.cfg.AllowGaps, false)
	for _, gap := range gaps {
		s.logger.Info("syncing gap", "start", gap.Start, "end", gap.End)
		pace := blocksync.NewPace("blocks synced", paceInterval, s.logger.With("sync_queue", "gap"))
		if s.status != nil {
			end := gap.End
			s.status.StartSegment("gap", gap.Start, &end)
		}
		err := rangeSyncer.SyncRange(ctx, gap.Start, gap.End, func(block blocksync.NewBlockData) error {
			return s.handleSyncedBlock(block, pace)
		})
		pace.Stop()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.logger.Error("gap sync stopped", "start", gap.Start, "end", gap.End, "error", err)
			continue
		}
		if s.status != nil {
			s.status.CompleteCurrentSegment()
		}
	}

	lastSynced := head
	s.logger.Info("initial sync complete", "synced_height", lastSynced)
	if s.status != nil {
		s.status.SetPhase("forward_sync")
		s.status.StartSegment("forward", lastSynced+1, nil)
	}

	pace := blocksync.NewPace("blocks synced", paceInterval, s.logger.With("sync_queue", "tip"))
	defer pace.Stop()

	forwardSyncer := blocksync.NewSyncer(s.client, s.logger, s.cfg.FetchJobs, false, false)
	err = forwardSyncer.SyncForward(ctx, lastSynced+1, func(block blocksync.NewBlockData) error {
		return s.handleSyncedBlock(block, pace)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		s.logger.Error("forward sync stopped", "start", lastSynced+1, "error", err)
		if s.status != nil {
			s.status.SetPhase("forward_sync_stopped")
		}
		return nil
	}

	return nil
}

// Resync reindexes the requested block ranges and exits once complete.
func (s *Syncer) Resync(ctx context.Context, targets []BlockRange) (ResyncStats, error) {
	defer gotracer.Trace(&ctx, txIndexerSyncerTraceTag)()

	var stats ResyncStats

	if s.indexer == nil {
		return stats, errors.New("tx indexer not configured")
	}
	if len(targets) == 0 {
		return stats, errors.New("no resync targets provided")
	}

	targets = NormalizeRanges(targets)

	rangeSyncer := blocksync.NewSyncer(s.client, s.logger, s.cfg.FetchJobs, false, false)
	for _, target := range targets {
		s.logger.Info("resyncing segment", "start", target.Start, "end", target.End)
		pace := blocksync.NewPace("blocks resynced", paceInterval, s.logger.With("sync_queue", "resync"))

		err := rangeSyncer.SyncRange(ctx, target.Start, target.End, func(block blocksync.NewBlockData) error {
			if block.Skipped {
				return fmt.Errorf("block %d was skipped during resync", block.Height)
			}
			indexStats, err := s.indexBlockForResync(block.Block, block.BlockResults.TxResults)
			if err != nil {
				if cleanupErr := s.cleanupFailedBlock(block.Height); cleanupErr != nil {
					return errors.Join(pkgerrors.Wrapf(err, "index block %d", block.Height), cleanupErr)
				}
				return pkgerrors.Wrapf(err, "index block %d", block.Height)
			}

			stats.BlocksSynced++
			stats.UniqueTxnsSeen += indexStats.IndexedEthTxs
			pace.Add(1)
			return nil
		})
		pace.Stop()
		if err != nil {
			return stats, err
		}
	}

	return stats, nil
}

func (s *Syncer) resolveEarliestBlock(ctx context.Context, requested int64) (int64, error) {
	defer gotracer.Trace(&ctx, txIndexerSyncerTraceTag)()

	block, err := s.client.Block(ctx, &requested)
	if err == nil {
		if block == nil || block.Block == nil {
			return 0, fmt.Errorf("validate earliest block %d: empty block response", requested)
		}
		return requested, nil
	}

	chainEarliest, ok := blocksync.LowestAvailableHeight(err)
	if !ok {
		return 0, pkgerrors.Wrapf(err, "validate earliest block %d", requested)
	}
	if !s.cfg.AllowGaps {
		return 0, fmt.Errorf("earliest block %d before chain earliest %d", requested, chainEarliest)
	}

	s.logger.Warn("earliest block before chain history; using chain earliest", "requested", requested, "chain_earliest", chainEarliest)
	return chainEarliest, nil
}

func toStatusRanges(ranges []BlockRange) []syncstatus.Range {
	if len(ranges) == 0 {
		return nil
	}
	out := make([]syncstatus.Range, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, syncstatus.Range{Start: r.Start, End: r.End})
	}
	return out
}

func (s *Syncer) handleSyncedBlock(block blocksync.NewBlockData, pace *blocksync.Pace) error {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, txIndexerSyncerTraceTag)()

	if block.Skipped {
		if s.status != nil {
			s.status.MarkBlock(block.Height, false)
		}
		pace.Add(1)
		return nil
	}
	if err := s.indexer.IndexBlock(block.Block, block.BlockResults.TxResults); err != nil {
		if cleanupErr := s.cleanupFailedBlock(block.Height); cleanupErr != nil {
			return errors.Join(fmt.Errorf("index block %d: %w", block.Height, err), cleanupErr)
		}
		if s.status != nil {
			s.status.MarkBlock(block.Height, false)
		}
		if errors.Is(err, ErrBlockParse) && s.cfg.AllowGaps {
			s.logger.Warn("skipping block with parsing errors", "height", block.Height, "error", err)
			pace.Add(1)
			return nil
		}
		return fmt.Errorf("index block %d: %w", block.Height, err)
	}
	if s.status != nil {
		s.status.MarkBlock(block.Height, true)
	}
	pace.Add(1)
	return nil
}

func (s *Syncer) indexBlockForResync(block *cmtypes.Block, txResults []*abci.ExecTxResult) (BlockIndexStats, error) {
	ctx := context.Background()
	defer gotracer.Traceless(&ctx, txIndexerSyncerTraceTag)()

	if withStats, ok := s.indexer.(txIndexerWithStats); ok {
		return withStats.IndexBlockWithStats(block, txResults)
	}
	if err := s.indexer.IndexBlock(block, txResults); err != nil {
		return BlockIndexStats{}, err
	}
	return BlockIndexStats{}, nil
}

func (s *Syncer) cleanupFailedBlock(height int64) error {
	if err := s.indexer.DeleteBlock(height); err != nil {
		return fmt.Errorf("delete failed block %d: %w", height, err)
	}
	return nil
}
