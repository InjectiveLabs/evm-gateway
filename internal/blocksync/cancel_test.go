package blocksync

import (
	"context"
	"errors"
	"testing"
)

// A block already fetched when the handler cancels the context must not be
// handled: select picks randomly among ready cases, so every delivery loop
// checks the context before calling the handler. A stream closed by the
// cancellation is reported as the cancellation.
func TestSyncLoopsStopHandlingBlocksOnCancel(t *testing.T) {
	loops := map[string]func(*Syncer, context.Context, Handler) error{
		"SyncRange": func(s *Syncer, ctx context.Context, h Handler) error {
			return s.SyncRange(ctx, 1, 1000, h)
		},
		"SyncForward": func(s *Syncer, ctx context.Context, h Handler) error {
			return s.SyncForward(ctx, 1, h)
		},
		"SyncHeights": func(s *Syncer, ctx context.Context, h Handler) error {
			return s.SyncHeights(ctx, []int64{1, 2, 3, 4, 5, 6, 7, 8}, h)
		},
	}
	for name, run := range loops {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				syncer := NewSyncer(&heightsClient{}, heightsTestLogger(), 4, false, false)
				ctx, cancel := context.WithCancel(context.Background())
				handled := 0
				err := run(syncer, ctx, func(NewBlockData) error {
					handled++
					cancel()
					return nil
				})
				cancel()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: err %v, want context.Canceled", i, err)
				}
				if handled != 1 {
					t.Fatalf("iteration %d: handled %d blocks after cancel, want 1", i, handled)
				}
			}
		})
	}
}
