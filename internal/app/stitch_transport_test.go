package app

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"google.golang.org/grpc/metadata"
)

type stitchRecordingTransport struct {
	mu   sync.Mutex
	seen map[string]string
}

func (r *stitchRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen[req.URL.Path] = req.Header.Get("x-stitch-backend")
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestStitchTransportPinsOnlyTheIndividualRequest(t *testing.T) {
	base := &stitchRecordingTransport{seen: make(map[string]string)}
	transport := stitchBackendTransport{base: base}
	var wg sync.WaitGroup
	for _, backend := range []string{"oldest", "newer", ""} {
		wg.Add(1)
		go func(backend string) {
			defer wg.Done()
			ctx := context.Background()
			if backend != "" {
				ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-stitch-backend", backend))
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://stitch/"+backend, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := transport.RoundTrip(req); err != nil {
				t.Error(err)
			}
			if req.Header.Get("x-stitch-backend") != "" {
				t.Error("transport mutated caller's request")
			}
		}(backend)
	}
	wg.Wait()
	for path, want := range map[string]string{"/oldest": "oldest", "/newer": "newer", "/": ""} {
		if got := base.seen[path]; got != want {
			t.Errorf("request %s pinned to %q, want %q", path, got, want)
		}
	}
}
