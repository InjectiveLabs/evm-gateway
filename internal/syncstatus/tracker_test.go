package syncstatus

import "testing"

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
