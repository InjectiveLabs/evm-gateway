package backend

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Run with STITCH_TEST_BINARY pointing to a locally built Stitch containing
// earliest discovery. The gateway and Stitch use their real gRPC wire paths;
// only the historical nodes are fixtures. No live chain or database is used.
func TestStitchEarliestCrossService(t *testing.T) {
	binary := os.Getenv("STITCH_TEST_BINARY")
	if binary == "" {
		t.Skip("set STITCH_TEST_BINARY to run the cross-service integration")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"nonce", "zero nonce", "absent account", "older shard unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			// Configured lower bounds deliberately order the faster/newer shard
			// first. The slower shard has the actual oldest retained EVM state.
			oldest := &earliestPeer{shard: true, floor: fixtureEarliestHeight, upper: 143000000, delay: 2 * time.Millisecond, sequence: 7}
			newer := &earliestPeer{shard: true, floor: 130000000, upper: 143000000, sequence: 99}
			switch scenario {
			case "zero nonce":
				oldest.sequence = 0
			case "absent account":
				oldest.accountErr = status.Error(codes.NotFound, "account not found")
			case "older shard unavailable":
				oldest.discoveryErr = status.Error(codes.Unavailable, "fixture older history offline")
			}
			addr := startStitchFixture(t, binary, oldest, newer)
			b := earliestBackend(t, addr, true)
			nonce, err := b.GetTransactionCount(common.Address{}, rpctypes.EthEarliestBlockNumber)
			if scenario == "older shard unavailable" {
				if err == nil || nonce != nil {
					t.Fatalf("incomplete cross-shard discovery became nonce: %v, %v", nonce, err)
				}
				for _, peer := range []*earliestPeer{oldest, newer} {
					for _, call := range peer.snapshot() {
						if call.method == "Account" {
							t.Fatal("queried account after incomplete discovery")
						}
					}
				}
				return
			}
			want := oldest.sequence
			if scenario == "absent account" {
				want = 0
			}
			if err != nil || nonce == nil || uint64(*nonce) != want {
				t.Fatalf("cross-service nonce=%v, error=%v; want %d", nonce, err, want)
			}
			accountCalls := 0
			for _, peer := range []*earliestPeer{oldest, newer} {
				paramsCalls := 0
				for _, call := range peer.snapshot() {
					for _, key := range []string{"stitch", "x-stitch-backend", "x-stitch-earliest-capability"} {
						if len(call.md.Get(key)) != 0 {
							t.Errorf("private header %s leaked to upstream: %+v", key, call)
						}
					}
					if call.method == "Params" {
						paramsCalls++
					}
					if call.method == "Account" {
						accountCalls++
						if peer != oldest || firstMD(call.md, "x-cosmos-block-height") != "127000123" {
							t.Errorf("dependent read did not use actual oldest state: %+v", call)
						}
					}
				}
				if paramsCalls < 2 {
					t.Fatalf("did not discover across both shards: %+v", peer.snapshot())
				}
			}
			if accountCalls != 1 {
				t.Fatalf("account queried %d times, want once", accountCalls)
			}
		})
	}
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startStitchFixture(t *testing.T, binary string, oldest, newer *earliestPeer) string {
	t.Helper()
	oldestAddr, newerAddr := startEarliestPeer(t, oldest), startEarliestPeer(t, newer)
	var checked atomic.Int64
	rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.Error(w, "unexpected RPC method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"sync_info":{"latest_block_height":"143000000","catching_up":false}}}`)
		checked.Add(1)
	}))
	t.Cleanup(rpcServer.Close)
	addr := unusedLoopbackAddress(t)
	configPath := filepath.Join(t.TempDir(), "stitch.yaml")
	cfg := fmt.Sprintf(`listen:
  grpc: { addr: %q }
log: { level: error, format: text }
policies:
  failover: { max_attempts: 1, per_attempt_timeout: 2s }
  circuit: { min_requests: 1000, error_threshold: 0.9, open_duration: 1s }
  health: { probe_interval: 100ms, max_lag_blocks: 0 }
  cache: { enabled: false }
backends:
  - name: fast-newer
    coverage: { kind: bounded, lower: 100000000, upper: 143000000 }
    weight: 1000
    endpoints: { rpc: %q, grpc: %q }
  - name: slow-oldest
    coverage: { kind: bounded, lower: 118886001, upper: 143000000 }
    weight: 1
    endpoints: { rpc: %q, grpc: %q }
`, addr, rpcServer.URL, newerAddr, rpcServer.URL, oldestAddr)
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "start", "--config", configPath)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("Stitch process output:\n%s", output.String())
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			if checked.Load() >= 2 {
				// The verifier publishes the snapshots just after consuming status.
				time.Sleep(20 * time.Millisecond)
				return addr
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Stitch did not start and verify its fixtures within 5 seconds")
	return ""
}
