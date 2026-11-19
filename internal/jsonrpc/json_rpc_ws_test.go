package jsonrpc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/gorilla/websocket"

	rpcstream "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/stream"
)

type websocketTestClient struct {
	conn       *websocket.Conn
	pings      <-chan struct{}
	messages   <-chan map[string]json.RawMessage
	errors     <-chan error
	serverDone <-chan struct{}
	stream     *rpcstream.RPCStream
}

func newWebsocketTestClient(t *testing.T, interval time.Duration) websocketTestClient {
	t.Helper()
	return newWebsocketTestClientWithDialer(t, interval, websocket.DefaultDialer)
}

func newWebsocketTestClientWithDialer(t *testing.T, interval time.Duration, dialer *websocket.Dialer) websocketTestClient {
	t.Helper()
	rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": "0x59f"})
	}))
	t.Cleanup(rpcServer.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stream := rpcstream.NewRPCStream(nil)
	server := &websocketsServer{
		rpcAddr:      strings.TrimPrefix(rpcServer.URL, "http://"),
		api:          newPubSubAPI(client.Context{}, logger, stream),
		logger:       logger,
		pingInterval: interval,
	}
	serverDone := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), http.Header{"Origin": {"https://testnet.helixapp.com"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Error("websocket handler did not stop its heartbeat after disconnect")
		}
	})
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	pings := make(chan struct{}, 128)
	pong := conn.PingHandler()
	conn.SetPingHandler(func(data string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return pong(data)
	})
	messages := make(chan map[string]json.RawMessage, 128)
	errors := make(chan error, 1)
	go func() {
		for {
			var message map[string]json.RawMessage
			if err := conn.ReadJSON(&message); err != nil {
				errors <- err
				return
			}
			select {
			case messages <- message:
			case <-serverDone:
				return
			}
		}
	}()
	return websocketTestClient{conn: conn, pings: pings, messages: messages, errors: errors, serverDone: serverDone, stream: stream}
}

func TestWebsocketHeartbeatKeepsSilentClientUsable(t *testing.T) {
	c := newWebsocketTestClient(t, 10*time.Millisecond)
	for range 3 {
		select {
		case <-c.pings:
		case err := <-c.errors:
			t.Fatalf("silent connection failed: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("server did not send a heartbeat to the silent client")
		}
	}
	// No application messages or client-initiated Pings preceded this request.
	if err := c.conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": "after-idle", "method": "eth_chainId", "params": []interface{}{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-c.messages:
		if string(response["id"]) != `"after-idle"` || string(response["result"]) != `"0x59f"` {
			t.Fatalf("unexpected response after heartbeats: %v", response)
		}
	case err := <-c.errors:
		t.Fatalf("connection failed after heartbeats: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no RPC response after heartbeats")
	}
}

func TestWebsocketHeartbeatWithActiveSubscription(t *testing.T) {
	c := newWebsocketTestClient(t, 5*time.Millisecond)
	if err := c.conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": "subscribe", "method": "eth_subscribe", "params": []string{"newHeads"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-c.messages:
		if string(response["id"]) != `"subscribe"` || response["result"] == nil {
			t.Fatalf("unexpected subscription response: %v", response)
		}
	case err := <-c.errors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("no subscription response")
	}
	stop := make(chan struct{})
	publisherDone := make(chan struct{})
	defer func() { close(stop); <-publisherDone }()
	go func() {
		defer close(publisherDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for height := int64(1); ; height++ {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.stream.HeaderStream().Add(rpcstream.RPCHeader{EthHeader: &ethtypes.Header{Number: big.NewInt(height), Difficulty: big.NewInt(0)}})
			}
		}
	}()
	deadline := time.After(3 * time.Second)
	pings, heads := 0, 0
	for pings < 3 || heads < 20 {
		select {
		case <-c.pings:
			pings++
		case message := <-c.messages:
			if string(message["method"]) != `"eth_subscription"` {
				t.Fatalf("invalid notification while heartbeats run: %v", message)
			}
			heads++
		case err := <-c.errors:
			t.Fatalf("concurrent subscription and heartbeat writes failed: %v", err)
		case <-deadline:
			t.Fatalf("received %d heads and %d heartbeats", heads, pings)
		}
	}
}

func TestWebsocketHeartbeatStopsImmediatelyOnDisconnect(t *testing.T) {
	c := newWebsocketTestClient(t, time.Hour)
	if err := c.conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect waited for the next heartbeat instead of canceling it")
	}
}

func TestWebsocketCloseDoesNotWaitForSubscriptionWriter(t *testing.T) {
	c := newWebsocketTestClient(t, time.Hour)
	peer := &wsConn{conn: c.conn, mux: new(sync.Mutex)}
	peer.mux.Lock()
	defer peer.mux.Unlock()
	done := make(chan error, 1)
	go func() { done <- peer.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close was blocked behind a subscription writer")
	}
}

// gatedWriteConn models an application frame that is still being transmitted
// when a heartbeat is due, without depending on operating-system buffer sizes.
type gatedWriteConn struct {
	net.Conn
	blockWrites atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
}

func (c *gatedWriteConn) Write(p []byte) (int, error) {
	if c.blockWrites.Load() {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		select {
		case <-c.release:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *gatedWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestWebsocketHeartbeatDoesNotInterruptSlowApplicationWrite(t *testing.T) {
	var gated *gatedWriteConn
	dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		gated = &gatedWriteConn{Conn: conn, entered: make(chan struct{}, 1), release: make(chan struct{}), closed: make(chan struct{})}
		return gated, nil
	}}
	c := newWebsocketTestClientWithDialer(t, time.Hour, dialer)
	peer := &wsConn{conn: c.conn, mux: new(sync.Mutex)}
	gated.blockWrites.Store(true)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gated.release) }) }
	defer release()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- peer.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": "slow-write", "method": "eth_chainId", "params": []interface{}{}})
	}()
	select {
	case <-gated.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("application frame did not reach the blocked network writer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); peer.heartbeat(ctx, 10*time.Millisecond) }()
	defer func() { cancel(); <-heartbeatDone }()
	select {
	case err := <-writeDone:
		t.Fatalf("heartbeat interrupted an active application write: %v", err)
	case <-c.serverDone:
		t.Fatal("heartbeat closed a connection with an active application write")
	case <-time.After(websocketControlWriteTimeout + 500*time.Millisecond):
	}
	release()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("application write failed after transmission resumed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("application write did not resume")
	}
	select {
	case response := <-c.messages:
		if string(response["id"]) != `"slow-write"` || string(response["result"]) != `"0x59f"` {
			t.Fatalf("unexpected response after slow application write: %v", response)
		}
	case err := <-c.errors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("connection was not usable after the slow application write")
	}
}
