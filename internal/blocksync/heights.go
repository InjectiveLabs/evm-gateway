package blocksync

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"upd.dev/xlab/gotracer"
)

// SyncHeights fetches an arbitrary set of heights with the syncer's parallel
// jobs and calls handler once per height in ascending order. Unlike SyncRange,
// which only parallelizes fetches within a contiguous range, scattered heights
// are fetched concurrently. Heights are deduplicated; at most a bounded window
// of fetched blocks is buffered ahead of the handler.
func (s *Syncer) SyncHeights(ctx context.Context, heights []int64, handler Handler) error {
	defer gotracer.Trace(&ctx, blockSyncTraceTag)()

	sorted, err := uniqueSortedHeights(heights)
	if err != nil {
		return err
	}
	if len(sorted) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	getter := &blockGetter{
		ctx:             ctx,
		client:          s.client,
		allowGaps:       s.allowGaps,
		fetchValidators: s.fetchValidators,
		logger:          s.logger,
		timingRecorder:  getFetchTimingRecorder(),
	}

	type fetched struct {
		index int
		data  NewBlockData
		err   error
	}

	window := s.jobs * 4
	// slots bounds the heights dispatched but not yet handled to the window,
	// so the next height to handle is always among the dispatched ones.
	slots := make(chan struct{}, window)
	results := make(chan fetched, window)
	indexes := make(chan int)

	go func() {
		defer close(indexes)
		for i := range sorted {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case indexes <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	for jobID := 0; jobID < s.jobs; jobID++ {
		go func(jobID int) {
			for i := range indexes {
				height := sorted[i]
				data, err := getter.fetchBlockByNum(ctx, uint64(height), jobID)
				if errors.Is(err, ErrBlockUnavailable) {
					data, err = NewBlockData{Height: height, Skipped: true}, nil
				}
				select {
				case results <- fetched{index: i, data: data, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}(jobID)
	}

	pending := make(map[int]fetched, window)
	next := 0
	for next < len(sorted) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			pending[result.index] = result
			for {
				ready, ok := pending[next]
				if !ok {
					break
				}
				delete(pending, next)
				if ready.err != nil {
					return fmt.Errorf("fetch block %d: %w", sorted[next], ready.err)
				}
				if !ready.data.Skipped {
					if err := handler(ready.data); err != nil {
						return err
					}
				}
				<-slots
				next++
			}
		}
	}
	return nil
}

func uniqueSortedHeights(heights []int64) ([]int64, error) {
	sorted := append([]int64(nil), heights...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]int64, 0, len(sorted))
	for _, height := range sorted {
		if height < 0 {
			return nil, fmt.Errorf("negative block height %d", height)
		}
		if len(out) > 0 && out[len(out)-1] == height {
			continue
		}
		out = append(out, height)
	}
	return out, nil
}
