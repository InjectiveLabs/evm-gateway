package jsonrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

// Regression coverage for Immunefi #68091 (IC-876): once a response cannot be
// written to a websocket peer, the server must stop reading from it. Before the
// fix, readLoop kept serving requests already buffered on the socket: every
// pipelined eth_subscribe opened another subscription and every other request
// was proxied to the JSON-RPC server, all for a peer that could not receive
// the results.

const (
	reqSubscribeNewHeads = `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`
	reqChainID           = `{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}`
)

// countingSubscriber replaces pubSubAPI and records subscription lifecycles.
type countingSubscriber struct {
	opened   atomic.Int32
	canceled atomic.Int32
}

func (c *countingSubscriber) subscribe(_ *wsConn, _ rpc.ID, params []interface{}) (context.CancelFunc, error) {
	if kind, _ := params[0].(string); kind != "newHeads" {
		return nil, errors.New("unsupported subscription")
	}
	c.opened.Add(1)
	var once sync.Once
	return func() { once.Do(func() { c.canceled.Add(1) }) }, nil
}

// failWritesConn makes every write fail once failWrites is set, while reads
// keep working. To the server this looks like a peer that went away with its
// requests still buffered on the socket.
type failWritesConn struct {
	net.Conn
	failWrites *atomic.Bool
}

func (c *failWritesConn) Write(p []byte) (int, error) {
	if c.failWrites.Load() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return c.Conn.Write(p)
}

type failWritesListener struct {
	net.Listener
	failWrites *atomic.Bool
}

func (l failWritesListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &failWritesConn{Conn: conn, failWrites: l.failWrites}, nil
}

type wsDeadPeerHarness struct {
	conn       *websocket.Conn
	subs       *countingSubscriber
	proxied    *atomic.Int32
	failWrites *atomic.Bool
	serverDone chan struct{}
}

// newWSDeadPeerHarness runs the production ServeHTTP handler behind a listener
// whose server-side writes can be made to fail, and dials one client.
func newWSDeadPeerHarness(t *testing.T) *wsDeadPeerHarness {
	t.Helper()

	proxied := new(atomic.Int32)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":"0x59f"}`)
	}))
	t.Cleanup(backend.Close)

	subs := &countingSubscriber{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &websocketsServer{
		rpcAddr:      strings.TrimPrefix(backend.URL, "http://"),
		api:          subs,
		logger:       logger,
		pingInterval: time.Hour, // keep heartbeats out of the picture
	}

	failWrites := new(atomic.Bool)
	serverDone := make(chan struct{})
	wsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		server.ServeHTTP(w, r)
	}))
	wsServer.Listener = failWritesListener{Listener: wsServer.Listener, failWrites: failWrites}
	wsServer.Start()
	t.Cleanup(wsServer.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &wsDeadPeerHarness{conn: conn, subs: subs, proxied: proxied, failWrites: failWrites, serverDone: serverDone}
}

// pipeline sends all requests with a single write on the client socket, the
// way a client pipelines a burst. Using one write means the client never races
// against the server closing the connection after the first request.
func (h *wsDeadPeerHarness) pipeline(t *testing.T, requests []string) {
	t.Helper()
	var burst bytes.Buffer
	for _, req := range requests {
		burst.Write(maskedTextFrame([]byte(req)))
	}
	if _, err := h.conn.NetConn().Write(burst.Bytes()); err != nil {
		t.Fatalf("pipeline requests: %v", err)
	}
}

// maskedTextFrame encodes a client-to-server text frame. The all-zero masking
// key is valid and leaves the payload unchanged.
func maskedTextFrame(payload []byte) []byte {
	frame := []byte{0x81} // FIN + text opcode
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, 0, 0, 0, 0) // masking key
	return append(frame, payload...)
}

// followUpRequests are queued behind the request whose response write fails.
// None of them may be served once the peer is known to be dead.
func followUpRequests() []string {
	requests := make([]string, 0, 16)
	for range 8 {
		requests = append(requests, reqSubscribeNewHeads, reqChainID)
	}
	return requests
}

func TestWebsocketStopsServingAfterResponseWriteFailure(t *testing.T) {
	tests := []struct {
		name        string
		request     string
		wantOpened  int32
		wantProxied int32
	}{
		{name: "eth_subscribe result", request: reqSubscribeNewHeads, wantOpened: 1},
		{name: "eth_subscribe error", request: `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["syncing"]}`},
		{name: "eth_subscribe empty params", request: `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":[]}`},
		{name: "eth_unsubscribe result", request: `{"jsonrpc":"2.0","id":1,"method":"eth_unsubscribe","params":["0x1"]}`},
		{name: "eth_unsubscribe invalid params", request: `{"jsonrpc":"2.0","id":1,"method":"eth_unsubscribe","params":[1]}`},
		{name: "malformed request", request: `{"jsonrpc":`},
		{name: "proxied request", request: reqChainID, wantProxied: 1},
		{name: "proxied batch", request: "[" + reqChainID + "]", wantProxied: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newWSDeadPeerHarness(t)
			h.failWrites.Store(true)
			h.pipeline(t, append([]string{tt.request}, followUpRequests()...))

			select {
			case <-h.serverDone:
			case <-time.After(5 * time.Second):
				t.Fatal("handler kept the connection open after a failed response write")
			}

			if got := h.subs.opened.Load(); got != tt.wantOpened {
				t.Errorf("opened %d subscriptions, want %d: pipelined eth_subscribe served for a dead peer", got, tt.wantOpened)
			}
			if opened, canceled := h.subs.opened.Load(), h.subs.canceled.Load(); canceled != opened {
				t.Errorf("canceled %d of %d subscriptions after the connection closed", canceled, opened)
			}
			if got := h.proxied.Load(); got != tt.wantProxied {
				t.Errorf("proxied %d requests, want %d: pipelined requests served for a dead peer", got, tt.wantProxied)
			}

			// The server must also have closed the socket.
			if err := h.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.conn.ReadMessage(); err == nil || isTimeout(err) {
				t.Fatalf("server did not close the connection: %v", err)
			}
		})
	}
}

// TestWebsocketServesPipelinedRequestsWhilePeerReceives guards the other side
// of the fix: error responses and pipelining alone must not close a healthy
// connection.
func TestWebsocketServesPipelinedRequestsWhilePeerReceives(t *testing.T) {
	h := newWSDeadPeerHarness(t)
	requests := append([]string{
		`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["syncing"]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_unsubscribe","params":["0x1"]}`,
		`{"jsonrpc":`,
		"[" + reqChainID + "]",
	}, followUpRequests()...)
	h.pipeline(t, requests)

	if err := h.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := range requests {
		if _, _, err := h.conn.ReadMessage(); err != nil {
			t.Fatalf("response %d of %d: %v", i+1, len(requests), err)
		}
	}
	select {
	case <-h.serverDone:
		t.Fatal("handler closed a connection whose peer receives responses")
	default:
	}
	if got, want := h.subs.opened.Load(), int32(8); got != want {
		t.Fatalf("opened %d subscriptions, want %d", got, want)
	}
	if got, want := h.proxied.Load(), int32(9); got != want {
		t.Fatalf("proxied %d requests, want %d", got, want)
	}

	// Closing the connection cancels every subscription it opened.
	_ = h.conn.Close()
	select {
	case <-h.serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not stop after the client disconnected")
	}
	if got := h.subs.canceled.Load(); got != 8 {
		t.Fatalf("canceled %d of 8 subscriptions after disconnect", got)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
