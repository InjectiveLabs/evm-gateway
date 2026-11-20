package e2e

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultDeadPeerGatewayRPCPort = 8746
	defaultDeadPeerGatewayWSPort  = 8747

	// deadPeerProbeCount is the number of requests pipelined behind the request
	// whose response write fails.
	deadPeerProbeCount = 200
	// deadPeerMaxServedProbes tolerates one probe served when the peer's RST
	// reaches the gateway only after the first response was already written.
	deadPeerMaxServedProbes = 1
)

// TestWebsocketDeadPeerAgainstGateway is the end-to-end regression test for
// Immunefi #68091 (IC-876). A client pipelines a request followed by a burst of
// requests in one TCP write, then aborts the connection with a RST, so the
// gateway fails to write the first response while the rest of the burst is
// still buffered on its socket. The gateway must stop serving the connection
// instead of draining the burst for a peer that cannot receive anything.
//
// Served requests are counted through the tx_by_hash cache-miss counter of
// /status/sync: every probe is an eth_getTransactionByHash for an unknown hash.
//
// The offline_rpc_only subtest needs only the gateway binary. The live_chain
// subtest also covers a successful eth_subscribe and runs when the Comet RPC
// from WEB3INJ_COMET_RPC is reachable.
func TestWebsocketDeadPeerAgainstGateway(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E") != "1" {
		t.Skip("set WEB3INJ_E2E=1 to run websocket dead peer e2e")
	}

	bin := buildGatewayBinary(t)
	rpcPort := getenvInt("WEB3INJ_E2E_DEAD_PEER_RPC_PORT", defaultDeadPeerGatewayRPCPort)
	wsPort := getenvInt("WEB3INJ_E2E_DEAD_PEER_WS_PORT", defaultDeadPeerGatewayWSPort)
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/", wsPort)

	common := []deadPeerTrigger{
		{name: "eth_unsubscribe", request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_unsubscribe","params":["0x1"]}`},
		{name: "eth_subscribe_invalid_params", request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_subscribe","params":[]}`},
		{name: "proxied_request", request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_chainId","params":[]}`},
		{name: "proxied_batch", request: `[{"jsonrpc":"2.0","id":"trigger","method":"eth_chainId","params":[]}]`},
	}

	t.Run("offline_rpc_only", func(t *testing.T) {
		proc := startGateway(t, gatewayStartConfig{
			BinaryPath:     bin,
			DataDir:        filepath.Join(t.TempDir(), "gateway"),
			RPCPort:        rpcPort,
			WSPort:         wsPort,
			Earliest:       1,
			FetchJobs:      1,
			CometRPC:       defaultCometRPC,
			GRPCAddr:       defaultGRPCAddr,
			ChainID:        "injective-1",
			EVMChainID:     "1776",
			EnableRPC:      true,
			OfflineRPCOnly: true,
			APIList:        "eth,net,web3",
			WaitTimeout:    60 * time.Second,
		})
		defer proc.Stop(t)

		// Without indexed streams, eth_subscribe is answered with an error, so
		// this covers the error-response write.
		triggers := append([]deadPeerTrigger{{
			name:    "eth_subscribe_unavailable",
			request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_subscribe","params":["newHeads"]}`,
		}}, common...)
		runDeadPeerScenarios(t, proc, wsURL, false, triggers)
	})

	t.Run("live_chain", func(t *testing.T) {
		ctx := context.Background()
		cometRPC := getenv("WEB3INJ_COMET_RPC", defaultCometRPC)
		chainID, err := cometChainID(ctx, cometRPC)
		if err != nil {
			t.Skipf("live chain unavailable at %s: %v", cometRPC, err)
		}
		head, err := cometLatestHeight(ctx, cometRPC)
		if err != nil {
			t.Fatalf("query comet head: %v", err)
		}

		proc := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     filepath.Join(t.TempDir(), "gateway"),
			RPCPort:     rpcPort,
			WSPort:      wsPort,
			Earliest:    maxInt64(1, head),
			FetchJobs:   2,
			CometRPC:    cometRPC,
			GRPCAddr:    resolveParityGRPCAddr(t),
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3",
			WaitTimeout: 90 * time.Second,
		})
		defer proc.Stop(t)

		triggers := append([]deadPeerTrigger{
			{name: "eth_subscribe", request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_subscribe","params":["newHeads"]}`},
			{name: "eth_subscribe_unsupported", request: `{"jsonrpc":"2.0","id":"trigger","method":"eth_subscribe","params":["syncing"]}`},
		}, common...)
		runDeadPeerScenarios(t, proc, wsURL, true, triggers)
	})
}

type deadPeerTrigger struct {
	name    string
	request string
}

func runDeadPeerScenarios(t *testing.T, proc *gatewayProcess, wsURL string, liveStreams bool, triggers []deadPeerTrigger) {
	t.Helper()

	t.Run("healthy_peer_keeps_connection", func(t *testing.T) {
		checkHealthyWebsocketSession(t, wsURL, liveStreams)
	})

	for _, trigger := range triggers {
		t.Run("aborted_peer_after_"+trigger.name, func(t *testing.T) {
			before := waitForStableTxByHashMisses(t, proc)

			requests := []string{trigger.request}
			for i := range deadPeerProbeCount {
				requests = append(requests, txByHashProbe(t, i))
			}
			pipelineAndAbort(t, wsURL, requests)

			served := waitForStableTxByHashMisses(t, proc) - before
			t.Logf("gateway served %d of %d pipelined probes after the peer aborted", served, deadPeerProbeCount)
			if served > deadPeerMaxServedProbes {
				t.Fatalf("gateway served %d of %d pipelined requests for a peer that could not receive responses (max %d)",
					served, deadPeerProbeCount, deadPeerMaxServedProbes)
			}

			// The gateway still serves new connections normally.
			checkHealthyWebsocketSession(t, wsURL, liveStreams)
		})
	}
}

// checkHealthyWebsocketSession verifies that a peer that reads its responses
// keeps a usable connection across successful and error responses.
func checkHealthyWebsocketSession(t *testing.T, wsURL string, liveStreams bool) {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	defer func() { _ = conn.Close() }()

	expectResult := func(id int, method string, params []interface{}) json.RawMessage {
		t.Helper()
		resp := wsRoundTrip(t, conn, id, method, params)
		if resp.Error != nil {
			t.Fatalf("%s returned error: %s", method, string(resp.Error))
		}
		return resp.Result
	}

	if result := expectResult(1, "eth_chainId", []interface{}{}); len(result) == 0 || string(result) == "null" {
		t.Fatalf("eth_chainId returned empty result")
	}
	if result := expectResult(2, "eth_unsubscribe", []interface{}{"0x1"}); string(result) != "false" {
		t.Fatalf("eth_unsubscribe of an unknown id returned %s, want false", string(result))
	}
	// An error response must not close a healthy connection.
	if resp := wsRoundTrip(t, conn, 3, "eth_subscribe", []interface{}{}); resp.Error == nil {
		t.Fatalf("eth_subscribe with empty params succeeded: %s", string(resp.Result))
	}

	if liveStreams {
		var subID string
		if err := json.Unmarshal(expectResult(4, "eth_subscribe", []interface{}{"newHeads"}), &subID); err != nil || !strings.HasPrefix(subID, "0x") {
			t.Fatalf("eth_subscribe newHeads returned invalid subscription id %q: %v", subID, err)
		}
		waitForSubscriptionNotification(t, conn, subID)
		if result := expectResult(5, "eth_unsubscribe", []interface{}{subID}); string(result) != "true" {
			t.Fatalf("eth_unsubscribe of an active subscription returned %s, want true", string(result))
		}
	} else if resp := wsRoundTrip(t, conn, 4, "eth_subscribe", []interface{}{"newHeads"}); resp.Error == nil {
		t.Fatalf("eth_subscribe without indexed streams succeeded: %s", string(resp.Result))
	}

	if result := expectResult(6, "eth_getTransactionByHash", []interface{}{randomHash(t)}); string(result) != "null" {
		t.Fatalf("eth_getTransactionByHash of an unknown hash returned %s, want null", string(result))
	}
}

type wsResponse struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Params struct {
		Subscription string `json:"subscription"`
	} `json:"params"`
}

// wsRoundTrip sends one request and returns its response. Responses that carry
// no id (error responses written by the websocket layer itself) are matched
// too, since the gateway answers requests in order.
func wsRoundTrip(t *testing.T, conn *websocket.Conn, id int, method string, params []interface{}) wsResponse {
	t.Helper()
	if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var resp wsResponse
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("read %s response: %v", method, err)
		}
		if resp.Method == "eth_subscription" {
			continue
		}
		if len(resp.ID) == 0 || string(resp.ID) == "null" || string(resp.ID) == fmt.Sprint(id) {
			return resp
		}
	}
}

func waitForSubscriptionNotification(t *testing.T, conn *websocket.Conn, subID string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg wsResponse
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("no newHeads notification for subscription %s: %v", subID, err)
		}
		if msg.Method == "eth_subscription" && msg.Params.Subscription == subID {
			return
		}
	}
}

// pipelineAndAbort writes every request in a single TCP write and then resets
// the connection without reading, which makes the gateway's response writes
// fail while the burst is still buffered on its socket.
func pipelineAndAbort(t *testing.T, wsURL string, requests []string) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	tcpConn, ok := conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("unexpected websocket transport %T", conn.NetConn())
	}

	var burst []byte
	for _, req := range requests {
		burst = append(burst, wsClientTextFrame([]byte(req))...)
	}
	if _, err := tcpConn.Write(burst); err != nil {
		t.Fatalf("write pipelined burst: %v", err)
	}
	if err := tcpConn.SetLinger(0); err != nil {
		t.Fatalf("set linger: %v", err)
	}
	if err := tcpConn.Close(); err != nil {
		t.Fatalf("abort connection: %v", err)
	}
}

// wsClientTextFrame encodes a client-to-server text frame. The all-zero masking
// key is valid and leaves the payload unchanged.
func wsClientTextFrame(payload []byte) []byte {
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

func txByHashProbe(t *testing.T, id int) string {
	t.Helper()
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionByHash","params":["%s"]}`, id, randomHash(t))
}

func randomHash(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "0x" + hex.EncodeToString(b[:])
}

// waitForStableTxByHashMisses returns the tx_by_hash cache-miss counter once it
// has stopped changing, so that requests still in flight are accounted for.
func waitForStableTxByHashMisses(t *testing.T, proc *gatewayProcess) uint64 {
	t.Helper()
	const settle = 1500 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)

	read := func() uint64 {
		st, err := proc.Status(context.Background())
		if err != nil {
			t.Fatalf("query gateway status: %v", err)
		}
		return st.Cache.TxByHash.Misses
	}

	last, lastChange := read(), time.Now()
	for time.Since(lastChange) < settle {
		if time.Now().After(deadline) {
			t.Fatalf("tx_by_hash misses did not settle, last value %d", last)
		}
		time.Sleep(100 * time.Millisecond)
		if v := read(); v != last {
			last, lastChange = v, time.Now()
		}
	}
	return last
}
