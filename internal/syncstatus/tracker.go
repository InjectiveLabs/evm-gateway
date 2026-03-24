package syncstatus

import (
	"sync"
	"sync/atomic"
	"time"
)

type Range struct {
	Start int64
	End   int64
}

type Segment struct {
	Type   string `json:"type"`
	Start  int64  `json:"start"`
	End    *int64 `json:"end,omitempty"`
	Cursor int64  `json:"cursor"`
}

type CacheLookupStats struct {
	Hits          uint64  `json:"hits"`
	Misses        uint64  `json:"misses"`
	LiveFallbacks uint64  `json:"live_fallbacks"`
	HitRatio      float64 `json:"hit_ratio"`
}

type CacheStats struct {
	TxByHash      CacheLookupStats `json:"tx_by_hash"`
	TxByIndex     CacheLookupStats `json:"tx_by_index"`
	ReceiptByHash CacheLookupStats `json:"receipt_by_hash"`
	BlockLogs     CacheLookupStats `json:"block_logs"`
}

type Snapshot struct {
	Threads           int        `json:"threads"`
	Phase             string     `json:"phase"`
	StartedAt         time.Time  `json:"started_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	EarliestBlock     int64      `json:"earliest_block"`
	ChainHead         int64      `json:"chain_head"`
	LastSyncedBlock   int64      `json:"last_synced_block"`
	LastSyncedAt      *time.Time `json:"last_synced_at,omitempty"`
	BlocksProcessed   int64      `json:"blocks_processed"`
	BlocksIndexed     int64      `json:"blocks_indexed"`
	BlocksPerSecond   float64    `json:"blocks_per_second"`
	AvgBlocksPerSec   float64    `json:"avg_blocks_per_second"`
	GapsTotal         int        `json:"gaps_total"`
	GapsRemaining     int        `json:"gaps_remaining"`
	CurrentSegment    *Segment   `json:"current_segment,omitempty"`
	LastSyncedSegment *Segment   `json:"last_synced_segment,omitempty"`
	Cache             CacheStats `json:"cache"`
}

type Tracker struct {
	threads int

	mu                sync.RWMutex
	phase             string
	startedAt         time.Time
	updatedAt         time.Time
	earliestBlock     int64
	chainHead         int64
	lastSyncedBlock   int64
	lastSyncedAt      time.Time
	blocksProcessed   int64
	blocksIndexed     int64
	gapsTotal         int
	gapsRemaining     int
	currentSegment    *Segment
	lastSyncedSegment *Segment
	recentBlocks      []time.Time

	txByHashHits           atomic.Uint64
	txByHashMisses         atomic.Uint64
	txByHashLiveFallbacks  atomic.Uint64
	txByIndexHits          atomic.Uint64
	txByIndexMisses        atomic.Uint64
	txByIndexLiveFallbacks atomic.Uint64
	receiptHits            atomic.Uint64
	receiptMisses          atomic.Uint64
	receiptLiveFallbacks   atomic.Uint64
	blockLogsHits          atomic.Uint64
	blockLogsMisses        atomic.Uint64
	blockLogsLiveFallbacks atomic.Uint64
}

func NewTracker(threads int, earliest int64) *Tracker {
	now := time.Now().UTC()
	return &Tracker{
		threads:         threads,
		phase:           "idle",
		startedAt:       now,
		updatedAt:       now,
		earliestBlock:   earliest,
		lastSyncedBlock: -1,
		recentBlocks:    make([]time.Time, 0, 256),
	}
}

func (t *Tracker) SetPhase(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = phase
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) SetEarliestBlock(height int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.earliestBlock = height
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) SetChainHead(height int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chainHead = height
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) SetGaps(gaps []Range) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gapsTotal = len(gaps)
	t.gapsRemaining = len(gaps)
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) StartSegment(kind string, start int64, end *int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentSegment = &Segment{
		Type:   kind,
		Start:  start,
		End:    end,
		Cursor: start - 1,
	}
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) CompleteCurrentSegment() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.currentSegment != nil {
		cp := *t.currentSegment
		t.lastSyncedSegment = &cp
		if cp.Type == "gap" && t.gapsRemaining > 0 {
			t.gapsRemaining--
		}
	}
	t.currentSegment = nil
	t.updatedAt = time.Now().UTC()
}

func (t *Tracker) MarkBlock(height int64, indexed bool) {
	now := time.Now().UTC()

	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastSyncedBlock = height
	if height > t.chainHead {
		t.chainHead = height
	}
	t.lastSyncedAt = now
	t.blocksProcessed++
	if indexed {
		t.blocksIndexed++
	}

	if t.currentSegment != nil && height > t.currentSegment.Cursor {
		t.currentSegment.Cursor = height
	}

	t.recentBlocks = append(t.recentBlocks, now)
	cutoff := now.Add(-30 * time.Second)
	keepFrom := 0
	for keepFrom < len(t.recentBlocks) && t.recentBlocks[keepFrom].Before(cutoff) {
		keepFrom++
	}
	if keepFrom > 0 {
		t.recentBlocks = append([]time.Time(nil), t.recentBlocks[keepFrom:]...)
	}

	t.updatedAt = now
}

func (t *Tracker) RecordTxByHashCacheHit() {
	t.txByHashHits.Add(1)
}

func (t *Tracker) RecordTxByHashCacheMiss() {
	t.txByHashMisses.Add(1)
}

func (t *Tracker) RecordTxByHashLiveFallback() {
	t.txByHashLiveFallbacks.Add(1)
}

func (t *Tracker) RecordTxByIndexCacheHit() {
	t.txByIndexHits.Add(1)
}

func (t *Tracker) RecordTxByIndexCacheMiss() {
	t.txByIndexMisses.Add(1)
}

func (t *Tracker) RecordTxByIndexLiveFallback() {
	t.txByIndexLiveFallbacks.Add(1)
}

func (t *Tracker) RecordReceiptCacheHit() {
	t.receiptHits.Add(1)
}

func (t *Tracker) RecordReceiptCacheMiss() {
	t.receiptMisses.Add(1)
}

func (t *Tracker) RecordReceiptLiveFallback() {
	t.receiptLiveFallbacks.Add(1)
}

func (t *Tracker) RecordBlockLogsCacheHit() {
	t.blockLogsHits.Add(1)
}

func (t *Tracker) RecordBlockLogsCacheMiss() {
	t.blockLogsMisses.Add(1)
}

func (t *Tracker) RecordBlockLogsLiveFallback() {
	t.blockLogsLiveFallbacks.Add(1)
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	s := Snapshot{
		Threads:         t.threads,
		Phase:           t.phase,
		StartedAt:       t.startedAt,
		UpdatedAt:       t.updatedAt,
		EarliestBlock:   t.earliestBlock,
		ChainHead:       t.chainHead,
		LastSyncedBlock: t.lastSyncedBlock,
		BlocksProcessed: t.blocksProcessed,
		BlocksIndexed:   t.blocksIndexed,
		GapsTotal:       t.gapsTotal,
		GapsRemaining:   t.gapsRemaining,
	}

	if t.currentSegment != nil {
		cp := *t.currentSegment
		s.CurrentSegment = &cp
	}
	if t.lastSyncedSegment != nil {
		cp := *t.lastSyncedSegment
		s.LastSyncedSegment = &cp
	}
	if !t.lastSyncedAt.IsZero() {
		ts := t.lastSyncedAt
		s.LastSyncedAt = &ts
	}

	elapsed := time.Since(t.startedAt).Seconds()
	if elapsed > 0 {
		s.AvgBlocksPerSec = float64(t.blocksProcessed) / elapsed
	}
	if n := len(t.recentBlocks); n > 1 {
		span := t.recentBlocks[n-1].Sub(t.recentBlocks[0]).Seconds()
		if span > 0 {
			s.BlocksPerSecond = float64(n-1) / span
		}
	}

	s.Cache.TxByHash = cacheLookupStats(
		t.txByHashHits.Load(),
		t.txByHashMisses.Load(),
		t.txByHashLiveFallbacks.Load(),
	)
	s.Cache.TxByIndex = cacheLookupStats(
		t.txByIndexHits.Load(),
		t.txByIndexMisses.Load(),
		t.txByIndexLiveFallbacks.Load(),
	)
	s.Cache.ReceiptByHash = cacheLookupStats(
		t.receiptHits.Load(),
		t.receiptMisses.Load(),
		t.receiptLiveFallbacks.Load(),
	)
	s.Cache.BlockLogs = cacheLookupStats(
		t.blockLogsHits.Load(),
		t.blockLogsMisses.Load(),
		t.blockLogsLiveFallbacks.Load(),
	)

	return s
}

func cacheLookupStats(hits, misses, liveFallbacks uint64) CacheLookupStats {
	stats := CacheLookupStats{
		Hits:          hits,
		Misses:        misses,
		LiveFallbacks: liveFallbacks,
	}
	total := hits + misses
	if total > 0 {
		stats.HitRatio = float64(hits) / float64(total)
	}
	return stats
}
