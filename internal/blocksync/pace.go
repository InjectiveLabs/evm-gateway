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
	doneC     chan struct{}
	stopOnce  sync.Once

	startedAt    time.Time
	lastReportAt time.Time
	lastCount    int64
	previous     float64
	stalled      time.Time
}

// NewPace starts a pace logger.
func NewPace(label string, timeframe time.Duration, logger *slog.Logger) *Pace {
	p := &Pace{
		label:        label,
		timeframe:    timeframe,
		logger:       logger,
		stopC:        make(chan struct{}),
		doneC:        make(chan struct{}),
		startedAt:    time.Now(),
		lastReportAt: time.Now(),
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
		<-p.doneC
	})
}

func (p *Pace) run() {
	ticker := time.NewTicker(p.timeframe)
	defer ticker.Stop()
	defer close(p.doneC)

	for {
		select {
		case <-ticker.C:
			p.report(time.Now(), false)
		case <-p.stopC:
			p.report(time.Now(), true)
			return
		}
	}
}

func (p *Pace) report(now time.Time, final bool) {
	current := p.count.Load()
	deltaCount := current - p.lastCount
	duration := now.Sub(p.lastReportAt)
	if duration <= 0 {
		duration = p.timeframe
	}
	if p.lastReportAt.Equal(p.startedAt) && duration < time.Millisecond {
		duration = time.Millisecond
	}

	if deltaCount > 0 {
		p.lastCount = current
		p.lastReportAt = now
		p.reportProgress(float64(deltaCount), duration, final)
		return
	}

	if final {
		return
	}

	switch {
	case p.previous == 0:
		return
	default:
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
	}
}

func (p *Pace) reportProgress(value float64, duration time.Duration, final bool) {
	p.previous = value
	p.stalled = time.Time{}

	floatFmt := func(f float64) string {
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
	secondsFmt := func(f float64) string {
		return strconv.FormatFloat(f, 'f', 0, 64) + "s"
	}

	rate := value / (float64(duration) / float64(time.Second))

	msg := p.label
	if final {
		msg = p.label + " [done]"
	}

	timeframe := (float64(duration) / float64(time.Second))

	p.logger.Info(msg, "count", floatFmt(value), "timeframe", secondsFmt(timeframe), "per_second", floatFmt(rate))
}
