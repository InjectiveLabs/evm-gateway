package blocksync

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bytedance/sonic"
)

const fetchTimingEnvVar = "WEB3INJ_DEBUG_BLOCKSYNC_TIMINGS_FILE"

type fetchTimingEvent struct {
	Timestamp  string  `json:"timestamp"`
	Kind       string  `json:"kind"`
	Height     int64   `json:"height"`
	Job        int     `json:"job"`
	Attempt    int     `json:"attempt"`
	DurationMS float64 `json:"duration_ms"`
	Success    bool    `json:"success"`
	Error      string  `json:"error,omitempty"`
}

type fetchTimingRecorder struct {
	mu   sync.Mutex
	file *os.File
}

var (
	fetchTimingOnce     sync.Once
	fetchTimingInstance *fetchTimingRecorder
)

func getFetchTimingRecorder() *fetchTimingRecorder {
	fetchTimingOnce.Do(func() {
		path := os.Getenv(fetchTimingEnvVar)
		if path == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		file, err := os.Create(path)
		if err != nil {
			return
		}
		fetchTimingInstance = &fetchTimingRecorder{file: file}
	})
	return fetchTimingInstance
}

func CloseFetchTimingRecorder() error {
	rec := getFetchTimingRecorder()
	if rec == nil || rec.file == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	err := rec.file.Close()
	rec.file = nil
	return err
}

func (r *fetchTimingRecorder) Record(kind string, height int64, jobID, attempt int, duration time.Duration, err error) {
	if r == nil {
		return
	}

	event := fetchTimingEvent{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Kind:       kind,
		Height:     height,
		Job:        jobID,
		Attempt:    attempt,
		DurationMS: float64(duration) / float64(time.Millisecond),
		Success:    err == nil,
	}
	if err != nil {
		event.Error = err.Error()
	}

	line, marshalErr := sonic.Marshal(event)
	if marshalErr != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return
	}
	_, _ = r.file.Write(append(line, '\n'))
}
