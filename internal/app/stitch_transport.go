package app

import (
	"net/http"

	"google.golang.org/grpc/metadata"
)

// stitchBackendTransport carries the selection for a concrete dependent read
// from its request context to Comet's HTTP transport. Latest and unrelated reads
// have no selection, and no routing marker is installed on the shared client.
type stitchBackendTransport struct {
	base http.RoundTripper
}

func (t stitchBackendTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	md, _ := metadata.FromOutgoingContext(req.Context())
	if backend := md.Get("x-stitch-backend"); len(backend) == 1 && backend[0] != "" {
		req = req.Clone(req.Context())
		req.Header.Set("x-stitch-backend", backend[0])
	}
	return t.base.RoundTrip(req)
}
