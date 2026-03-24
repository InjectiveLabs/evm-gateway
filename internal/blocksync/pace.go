package blocksync

import (
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Pace logs a running rate based on a fixed timeframe.
type Pace struct {
	label     string
	timeframe time.Duration
	logger    *slog.Logger
	count     atomic.Int64
	stopC     chan struct{}
	stopOnce  sync.Once

	previous float64
	stalled  time.Time
}

// NewPace starts a pace logger.
func NewPace(label string, timeframe time.Duration, logger *slog.Logger) *Pace {
	p := &Pace{
		label:     label,
		timeframe: timeframe,
		logger:    logger,
		stopC:     make(chan struct{}),
	}
	go p.run()
	return p
}

// Add increments the pace counter.
func (p *Pace) Add(n int64) {
	p.count.Add(n)
}

// Stop stops the pace logger.
func (p *Pace) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopC)
	})
}

func (p *Pace) run() {
	ticker := time.NewTicker(p.timeframe)
	defer ticker.Stop()

	var last int64
	for {
		select {
		case <-ticker.C:
			current := p.count.Load()
			delta := current - last
			last = current
			p.report(float64(delta))
		case <-p.stopC:
			return
		}
	}
}

func (p *Pace) report(value float64) {
	switch {
	case value == 0 && p.previous == 0:
		return
	case value == 0 && p.previous != 0:
		dur := p.timeframe
		if !p.stalled.IsZero() {
			dur = time.Since(p.stalled)
			n := dur / p.timeframe
			if dur-n*p.timeframe < 10*time.Millisecond {
				dur = n * p.timeframe
			}
		} else {
			p.stalled = time.Now().Add(-dur)
		}
		p.logger.Warn("pace stalled", "label", p.label, "duration", dur)
		return
	default:
		p.previous = value
		p.stalled = time.Time{}
	}

	floatFmt := func(f float64) string {
		return strconv.FormatFloat(f, 'f', 3, 64)
	}
	rate := value / (float64(p.timeframe) / float64(time.Second))
	p.logger.Info("pace", "label", p.label, "count", floatFmt(value), "timeframe", p.timeframe, "per_second", floatFmt(rate))
}
