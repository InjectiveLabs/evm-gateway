package syncstatus

import (
	"testing"
	"time"
)

func TestTrackerMarkBlockDoesNotRegressLastSyncedBlock(t *testing.T) {
	tracker := NewTracker(4, 1)

	tracker.MarkBlock(100, true)
	tracker.MarkBlock(10, true)

	snapshot := tracker.Snapshot()
	if snapshot.LastSyncedBlock != 100 {
		t.Fatalf("unexpected last synced block: got %d want 100", snapshot.LastSyncedBlock)
	}
	if snapshot.BlocksProcessed != 2 {
		t.Fatalf("unexpected blocks processed: got %d want 2", snapshot.BlocksProcessed)
	}
}

func TestTrackerCompleteGapKeepsCurrentForwardSegment(t *testing.T) {
	tracker := NewTracker(4, 1)
	tracker.SetGaps([]Range{{Start: 1, End: 10}, {Start: 20, End: 30}})
	tracker.StartSegment("forward", 100, nil)

	tracker.CompleteGap(1, 10)

	snapshot := tracker.Snapshot()
	if snapshot.GapsRemaining != 1 {
		t.Fatalf("unexpected gaps remaining: got %d want 1", snapshot.GapsRemaining)
	}
	if snapshot.CurrentSegment == nil || snapshot.CurrentSegment.Type != "forward" {
		t.Fatalf("expected current forward segment, got %#v", snapshot.CurrentSegment)
	}
	if snapshot.LastSyncedSegment == nil || snapshot.LastSyncedSegment.Type != "gap" {
		t.Fatalf("expected last synced gap segment, got %#v", snapshot.LastSyncedSegment)
	}
}

func TestTrackerCaughtUp(t *testing.T) {
	var none *Tracker
	if none.CaughtUp(time.Minute) {
		t.Fatalf("nil tracker caught up")
	}

	tracker := NewTracker(4, 1)
	if tracker.CaughtUp(time.Minute) {
		t.Fatalf("idle tracker caught up")
	}
	tracker.SetPhase("initial_gap_sync")
	tracker.MarkBlock(10, true)
	if tracker.CaughtUp(time.Minute) {
		t.Fatalf("caught up during initial gap sync")
	}

	tracker.SetPhase(PhaseForwardSync)
	if !tracker.CaughtUp(time.Minute) {
		t.Fatalf("forward sync with a fresh block not caught up")
	}

	tracker.mu.Lock()
	tracker.lastSyncedAt = time.Now().Add(-2 * time.Minute)
	tracker.mu.Unlock()
	if tracker.CaughtUp(time.Minute) {
		t.Fatalf("stalled forward sync caught up")
	}
	tracker.MarkBlock(11, true)
	if !tracker.CaughtUp(time.Minute) {
		t.Fatalf("resumed forward sync not caught up")
	}

	// a gap that failed to sync stays remaining
	tracker.SetGaps([]Range{{Start: 1, End: 5}})
	if tracker.CaughtUp(time.Minute) {
		t.Fatalf("caught up with a gap left")
	}

	parallel := NewTracker(4, 1)
	parallel.SetPhase(PhaseParallelGapTipSync)
	parallel.SetGaps([]Range{{Start: 1, End: 5}})
	parallel.MarkBlock(100, true)
	if parallel.CaughtUp(time.Minute) {
		t.Fatalf("parallel sync caught up before its gaps")
	}
	parallel.CompleteGap(1, 5)
	if !parallel.CaughtUp(time.Minute) {
		t.Fatalf("parallel sync with gaps done not caught up")
	}
}
