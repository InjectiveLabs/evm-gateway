package blocksync

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPaceStopFlushesFinalProgressAtInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	pace := NewPace("blocks synced", time.Hour, logger)
	pace.Add(3)
	time.Sleep(10 * time.Millisecond)
	pace.Stop()

	out := buf.String()
	if !strings.Contains(out, "blocks synced [done]") {
		t.Fatalf("expected final pace log, got %q", out)
	}
}
