package blocksync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
)

// heightsClient serves synthetic blocks, with per-height fetch delays to make
// fetches complete out of order, and tracks fetch concurrency.
type heightsClient struct {
	delays    map[int64]time.Duration
	failAt    map[int64]error
	inFlight  atomic.Int64
	maxFlight atomic.Int64
	mu        sync.Mutex
	fetched   []int64
}

func (c *heightsClient) enter() func() {
	n := c.inFlight.Add(1)
	for {
		current := c.maxFlight.Load()
		if n <= current || c.maxFlight.CompareAndSwap(current, n) {
			break
		}
	}
	return func() { c.inFlight.Add(-1) }
}

func (c *heightsClient) Block(ctx context.Context, height *int64) (*ctypes.ResultBlock, error) {
	defer c.enter()()
	c.mu.Lock()
	c.fetched = append(c.fetched, *height)
	c.mu.Unlock()
	if err := c.failAt[*height]; err != nil {
		return nil, err
	}
	select {
	case <-time.After(c.delays[*height]):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &ctypes.ResultBlock{Block: &tmtypes.Block{Header: tmtypes.Header{Height: *height}}}, nil
}

func (c *heightsClient) BlockResults(_ context.Context, height *int64) (*ctypes.ResultBlockResults, error) {
	if err := c.failAt[*height]; err != nil {
		return nil, err
	}
	return &ctypes.ResultBlockResults{Height: *height}, nil
}

func (c *heightsClient) Validators(context.Context, *int64, *int, *int) (*ctypes.ResultValidators, error) {
	return nil, errors.New("unexpected Validators call")
}

func heightsTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncHeightsDeliversScatteredHeightsInOrderConcurrently(t *testing.T) {
	// Lower heights are slower, so fetches complete in reverse order.
	client := &heightsClient{delays: map[int64]time.Duration{
		100:  80 * time.Millisecond,
		7:    60 * time.Millisecond,
		5000: 40 * time.Millisecond,
		42:   20 * time.Millisecond,
	}}
	syncer := NewSyncer(client, heightsTestLogger(), 4, false, false)

	var handled []int64
	err := syncer.SyncHeights(context.Background(), []int64{5000, 42, 7, 100, 42, 7}, func(block NewBlockData) error {
		if block.Block == nil || block.Block.Height != block.Height || block.BlockResults == nil || block.BlockResults.Height != block.Height {
			t.Fatalf("unexpected block data for %d: %+v", block.Height, block)
		}
		handled = append(handled, block.Height)
		return nil
	})
	if err != nil {
		t.Fatalf("SyncHeights: %v", err)
	}
	want := []int64{7, 42, 100, 5000}
	if len(handled) != len(want) {
		t.Fatalf("handled %v want %v", handled, want)
	}
	for i := range want {
		if handled[i] != want[i] {
			t.Fatalf("handled %v want %v", handled, want)
		}
	}
	if len(client.fetched) != len(want) {
		t.Fatalf("duplicate heights must be fetched once, fetched %v", client.fetched)
	}
	if client.maxFlight.Load() < 2 {
		t.Fatalf("scattered heights must be fetched concurrently, max in flight %d", client.maxFlight.Load())
	}
}

func TestSyncHeightsBoundsBufferedBlocks(t *testing.T) {
	// The first height is slow: workers must stop dispatching once the window
	// of fetched-but-unhandled blocks is full.
	const jobs = 2
	heights := make([]int64, 0, 50)
	delays := map[int64]time.Duration{1: 100 * time.Millisecond}
	for h := int64(1); h <= 50; h++ {
		heights = append(heights, h)
	}
	client := &heightsClient{delays: delays}
	syncer := NewSyncer(client, heightsTestLogger(), jobs, false, false)

	var maxFetchedAhead int
	err := syncer.SyncHeights(context.Background(), heights, func(block NewBlockData) error {
		client.mu.Lock()
		ahead := len(client.fetched) - int(block.Height)
		client.mu.Unlock()
		if ahead > maxFetchedAhead {
			maxFetchedAhead = ahead
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SyncHeights: %v", err)
	}
	if maxFetchedAhead > jobs*4 {
		t.Fatalf("fetched %d blocks ahead of the handler, window is %d", maxFetchedAhead, jobs*4)
	}
}

func TestSyncHeightsEdgeCases(t *testing.T) {
	syncer := NewSyncer(&heightsClient{}, heightsTestLogger(), 2, false, false)

	if err := syncer.SyncHeights(context.Background(), nil, func(NewBlockData) error {
		t.Fatalf("handler must not run for no heights")
		return nil
	}); err != nil {
		t.Fatalf("empty heights: %v", err)
	}

	if err := syncer.SyncHeights(context.Background(), []int64{3, -1}, nil); err == nil || !strings.Contains(err.Error(), "negative block height") {
		t.Fatalf("expected negative height error, got %v", err)
	}
}

func TestSyncHeightsPropagatesErrors(t *testing.T) {
	t.Run("fetch", func(t *testing.T) {
		client := &heightsClient{failAt: map[int64]error{9: errors.New("archive down")}}
		syncer := NewSyncer(client, heightsTestLogger(), 2, false, false)
		var handled []int64
		err := syncer.SyncHeights(context.Background(), []int64{3, 9, 12}, func(block NewBlockData) error {
			handled = append(handled, block.Height)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "fetch block 9") || !strings.Contains(err.Error(), "archive down") {
			t.Fatalf("expected fetch error, got %v", err)
		}
		if len(handled) != 1 || handled[0] != 3 {
			t.Fatalf("heights before the failure must be handled in order, handled %v", handled)
		}
	})

	t.Run("handler", func(t *testing.T) {
		syncer := NewSyncer(&heightsClient{}, heightsTestLogger(), 2, false, false)
		err := syncer.SyncHeights(context.Background(), []int64{1, 2, 3}, func(block NewBlockData) error {
			if block.Height == 2 {
				return errors.New("index failed")
			}
			return nil
		})
		if err == nil || err.Error() != "index failed" {
			t.Fatalf("expected handler error, got %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		client := &heightsClient{delays: map[int64]time.Duration{1: time.Hour}}
		syncer := NewSyncer(client, heightsTestLogger(), 2, false, false)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		if err := syncer.SyncHeights(ctx, []int64{1, 2, 3}, func(NewBlockData) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	})
}

func TestSyncHeightsSkipsUnavailableBlocksWhenGapsAllowed(t *testing.T) {
	client := &heightsClient{failAt: map[int64]error{5: errors.New("height 5 is not available, lowest height is 6")}}
	syncer := NewSyncer(client, heightsTestLogger(), 2, true, false)
	var handled []int64
	err := syncer.SyncHeights(context.Background(), []int64{5, 8}, func(block NewBlockData) error {
		handled = append(handled, block.Height)
		return nil
	})
	if err != nil {
		t.Fatalf("SyncHeights: %v", err)
	}
	if len(handled) != 1 || handled[0] != 8 {
		t.Fatalf("unavailable height must be skipped, handled %v", handled)
	}
}
