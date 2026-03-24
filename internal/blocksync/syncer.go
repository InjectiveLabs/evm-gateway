package blocksync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
)

const (
	retryAttempts = 10
	retryDelay    = 300 * time.Millisecond

	// https://github.com/cometbft/cometbft/blob/v0.37.4/rpc/core/env.go#L193
	NotFoundErr = "is not available"
)

var ErrBlockUnavailable = errors.New("block not available")

// BlockClient exposes the RPC calls needed for block sync.
type BlockClient interface {
	Block(ctx context.Context, height *int64) (*ctypes.ResultBlock, error)
	BlockResults(ctx context.Context, height *int64) (*ctypes.ResultBlockResults, error)
	Validators(ctx context.Context, height *int64, page, perPage *int) (*ctypes.ResultValidators, error)
}

// NewBlockData represents a fetched block with metadata.
type NewBlockData struct {
	Height       int64
	Block        *tmtypes.Block
	BlockResults *ctypes.ResultBlockResults
	ActiveSet    []*tmtypes.Validator
	Skipped      bool
}

// Handler processes a fetched block.
type Handler func(NewBlockData) error

// Syncer streams blocks in order using concurrent fetch jobs.
type Syncer struct {
	client          BlockClient
	logger          *slog.Logger
	jobs            int
	allowGaps       bool
	fetchValidators bool
}

func NewSyncer(client BlockClient, logger *slog.Logger, jobs int, allowGaps, fetchValidators bool) *Syncer {
	if jobs <= 0 {
		jobs = 1
	}
	return &Syncer{
		client:          client,
		logger:          logger,
		jobs:            jobs,
		allowGaps:       allowGaps,
		fetchValidators: fetchValidators,
	}
}

// SyncRange syncs blocks from start to end, inclusive, in ascending order.
func (s *Syncer) SyncRange(ctx context.Context, start, end int64, handler Handler) error {
	if start <= 0 {
		start = 1
	}
	if end < start {
		return nil
	}

	getter := newBlockGetter(ctx, s.client, s.logger, uint64(start), s.jobs, BlockGetterDirectionForward, s.allowGaps, s.fetchValidators)
	defer getter.Close()

	expected := start
	newBlocks := getter.NewBlockDataChan()

	for expected <= end {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case nextBlock, ok := <-newBlocks:
			if !ok {
				return errors.New("block stream closed")
			}
			if nextBlock.Height != expected {
				return fmt.Errorf("sequential block height mismatch: expected %d, got %d", expected, nextBlock.Height)
			}
			if !nextBlock.Skipped {
				if err := handler(nextBlock); err != nil {
					return err
				}
			}
			expected++
		}
	}

	return nil
}

// SyncForward syncs blocks from start forward until the context is canceled.
func (s *Syncer) SyncForward(ctx context.Context, start int64, handler Handler) error {
	if start <= 0 {
		start = 1
	}

	getter := newBlockGetter(ctx, s.client, s.logger, uint64(start), s.jobs, BlockGetterDirectionForward, false, s.fetchValidators)
	defer getter.Close()

	expected := start
	newBlocks := getter.NewBlockDataChan()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case nextBlock, ok := <-newBlocks:
			if !ok {
				return errors.New("block stream closed")
			}
			if nextBlock.Height != expected {
				return fmt.Errorf("sequential block height mismatch: expected %d, got %d", expected, nextBlock.Height)
			}
			if !nextBlock.Skipped {
				if err := handler(nextBlock); err != nil {
					return err
				}
			}
			expected++
		}
	}
}

type BlockGetterDirection string

const (
	BlockGetterDirectionForward  BlockGetterDirection = "forward"
	BlockGetterDirectionBackward BlockGetterDirection = "backward"
)

type blockGetter struct {
	ctx             context.Context
	client          BlockClient
	jobs            int
	jobMux          *sync.RWMutex
	jobCond         *sync.Cond
	height          uint64
	direction       BlockGetterDirection
	allowGaps       bool
	fetchValidators bool

	newBlocksC   chan NewBlockData
	newBlocksMap map[uint64]NewBlockData
	closeC       chan struct{}
	closeOnce    sync.Once

	logger *slog.Logger
}

func newBlockGetter(
	ctx context.Context,
	client BlockClient,
	logger *slog.Logger,
	initHeight uint64,
	parallelJobs int,
	direction BlockGetterDirection,
	allowGaps bool,
	fetchValidators bool,
) *blockGetter {
	getter := &blockGetter{
		ctx:             ctx,
		client:          client,
		jobs:            parallelJobs,
		jobMux:          new(sync.RWMutex),
		jobCond:         sync.NewCond(new(sync.Mutex)),
		height:          initHeight,
		direction:       direction,
		allowGaps:       allowGaps,
		fetchValidators: fetchValidators,
		newBlocksC:      make(chan NewBlockData, parallelJobs*100),
		newBlocksMap:    make(map[uint64]NewBlockData, parallelJobs*100),
		closeC:          make(chan struct{}),
		logger:          logger,
	}

	logger.Debug("initBlockGetter ready to announce and pull blocks")

	go getter.announceBlocks(initHeight)
	go getter.pullBlocks()

	return getter
}

func (b *blockGetter) Close() {
	b.closeOnce.Do(func() {
		if b.jobCond != nil {
			b.jobCond.Signal()
		}
		close(b.closeC)
	})
}

func (b *blockGetter) NewBlockDataChan() <-chan NewBlockData {
	return b.newBlocksC
}

func (b *blockGetter) isDone() bool {
	select {
	case <-b.closeC:
		return true
	case <-b.ctx.Done():
		return true
	default:
		return false
	}
}

func (b *blockGetter) announceBlocks(startHeight uint64) {
	height := startHeight
	if b.direction == BlockGetterDirectionBackward {
		height = startHeight - 1
	}

	for {
		if b.isDone() {
			close(b.newBlocksC)
			return
		}

		b.jobCond.L.Lock()
		ev, found := b.newBlocksMap[height]
		for !found {
			if b.isDone() {
				b.jobCond.L.Unlock()
				close(b.newBlocksC)
				return
			}

			b.jobCond.Wait()
			ev, found = b.newBlocksMap[height]
		}
		delete(b.newBlocksMap, height)
		b.jobCond.L.Unlock()

		b.newBlocksC <- ev

		if b.direction == BlockGetterDirectionForward {
			height++
			continue
		}
		if b.direction == BlockGetterDirectionBackward {
			height--
			if height == 0 {
				b.logger.Warn("backward sync done; reached 0 block height")
				close(b.newBlocksC)
				return
			}
			continue
		}

		b.logger.Error("unsupported block getter direction", "direction", b.direction)
		close(b.newBlocksC)
		return
	}
}

func (b *blockGetter) pullBlocks() {
	wg := new(sync.WaitGroup)
	defer wg.Wait()

	for i := 0; i < b.jobs; i++ {
		wg.Add(1)
		go func(jobID int) {
			defer wg.Done()

			getHeightToFetch := func() uint64 {
				b.jobMux.Lock()
				defer b.jobMux.Unlock()

				h := b.height
				switch b.direction {
				case BlockGetterDirectionForward:
					b.height++
				case BlockGetterDirectionBackward:
					b.height--
				default:
					b.logger.Error("unsupported block getter direction", "direction", b.direction)
				}

				return h
			}

			height := getHeightToFetch()

			for {
				if b.isDone() {
					return
				}

				jobLog := b.logger.With("job", jobID, "height", height)

				newBlock, err := b.fetchBlockByNum(b.ctx, height)
				if err != nil {
					if errors.Is(err, ErrBlockUnavailable) && b.allowGaps {
						newBlock = NewBlockData{Height: int64(height), Skipped: true}
					} else {
						jobLog.Warn("failed to fully fetch block, retry in 1s", "error", err)
						time.Sleep(1 * time.Second)
						continue
					}
				}

				var tooFarFromHead bool

				b.jobCond.L.Lock()
				b.newBlocksMap[height] = newBlock
				b.jobCond.Signal()
				if len(b.newBlocksMap) > 1024*b.jobs {
					tooFarFromHead = true
				}
				b.jobCond.L.Unlock()

				for tooFarFromHead {
					time.Sleep(200 * time.Millisecond)
					b.jobCond.L.Lock()
					tooFarFromHead = len(b.newBlocksMap) > 1024*b.jobs
					b.jobCond.L.Unlock()
				}

				height = getHeightToFetch()
			}
		}(i)
	}
}

func (b *blockGetter) fetchBlockByNum(ctx context.Context, height uint64) (NewBlockData, error) {
	blockC := make(chan *ctypes.ResultBlock, 1)
	blockResultsC := make(chan *ctypes.ResultBlockResults, 1)
	validatorSetC := make(chan []*tmtypes.Validator, 1)
	errC := make(chan error, 3)

	height64 := int64(height)

	go func() {
		defer close(blockC)
		if err := b.fetchWithRetry(ctx, func() error {
			block, err := b.client.Block(ctx, &height64)
			if err != nil {
				return err
			}
			if block == nil {
				return fmt.Errorf("failed to get block info (%d)", height)
			}
			blockC <- block
			return nil
		}); err != nil {
			errC <- err
		}
	}()

	go func() {
		defer close(blockResultsC)
		if err := b.fetchWithRetry(ctx, func() error {
			blockResults, err := b.client.BlockResults(ctx, &height64)
			if err != nil {
				return err
			}
			if blockResults == nil {
				return fmt.Errorf("failed to get block results (%d)", height)
			}
			blockResultsC <- blockResults
			return nil
		}); err != nil {
			errC <- err
		}
	}()

	go func() {
		defer close(validatorSetC)
		if !b.fetchValidators {
			return
		}
		if err := b.fetchWithRetry(ctx, func() error {
			perPage := 200
			validatorSet, err := b.client.Validators(ctx, &height64, nil, &perPage)
			if err != nil {
				return err
			}
			if validatorSet == nil {
				return fmt.Errorf("failed to get validator set (%d)", height)
			}
			validatorSetC <- validatorSet.Validators
			return nil
		}); err != nil {
			errC <- err
		}
	}()

	block := <-blockC
	blockResults := <-blockResultsC
	var validators []*tmtypes.Validator
	if b.fetchValidators {
		validators = <-validatorSetC
	}

	select {
	case err := <-errC:
		return NewBlockData{}, err
	default:
	}

	return NewBlockData{
		Height:       int64(height),
		Block:        block.Block,
		BlockResults: blockResults,
		ActiveSet:    validators,
	}, nil
}

func (b *blockGetter) fetchWithRetry(ctx context.Context, fn func() error) error {
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fn()
		if err == nil {
			return nil
		}
		if b.allowGaps && isNotFound(err) {
			return ErrBlockUnavailable
		}
		if attempt == retryAttempts-1 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), NotFoundErr)
}
