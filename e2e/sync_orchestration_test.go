package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
)

const (
	defaultSourceRPC = "http://127.0.0.1:8545"
	defaultCometRPC  = "http://127.0.0.1:26657"
	defaultGRPCAddr  = "127.0.0.1:9090"
)

func TestSyncOrchestration(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E") != "1" {
		t.Skip("set WEB3INJ_E2E=1 to run sync orchestration e2e")
	}

	sourceRPC := getenv("WEB3INJ_E2E_SOURCE_RPC", defaultSourceRPC)
	cometRPC := getenv("WEB3INJ_COMET_RPC", defaultCometRPC)
	grpcAddr := getenv("WEB3INJ_GRPC_ADDR", defaultGRPCAddr)
	chainID, err := cometChainID(context.Background(), cometRPC)
	if err != nil {
		t.Fatalf("query chain id: %v", err)
	}

	head, err := ethBlockNumber(context.Background(), sourceRPC)
	if err != nil {
		t.Logf("eth source rpc unavailable (%v), using comet status head from %s", err, cometRPC)
		head, err = cometLatestHeight(context.Background(), cometRPC)
		if err != nil {
			t.Fatalf("query head: %v", err)
		}
		sourceRPC = ""
	}

	window := int64(80)
	earliest := maxInt64(1, head-window)

	bin := buildGatewayBinary(t)
	t.Logf("source head=%d earliest=%d", head, earliest)

	var candidates []txCandidate
	if sourceRPC != "" {
		candidates, err = discoverTxCandidates(context.Background(), sourceRPC, head, 500)
		if err != nil {
			t.Fatalf("discover tx candidates: %v", err)
		}
	}
	recentTx, oldTx := pickTxCandidates(candidates, earliest)

	t.Run("scenario1_sync_from_beginning", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "scenario1")
		proc := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18545,
			WSPort:      18546,
			Earliest:    earliest,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})
		defer proc.Stop(t)

		waitForCondition(t, 4*time.Minute, func() (bool, error) {
			st, err := proc.Status(context.Background())
			if err != nil {
				return false, err
			}
			if _, err := web3ClientVersion(context.Background(), proc.RPCURL()); err != nil {
				return false, err
			}
			return st.GapsRemaining == 0 && st.LastSyncedBlock >= head, nil
		}, "scenario1 did not reach forward sync with zero gaps")

		st, err := proc.Status(context.Background())
		if err == nil {
			t.Logf("scenario1 metrics: head=%d last=%d gaps=%d/%d blocks=%d indexed=%d bps=%.3f avg_bps=%.3f",
				st.ChainHead, st.LastSyncedBlock, st.GapsRemaining, st.GapsTotal,
				st.BlocksProcessed, st.BlocksIndexed, st.BlocksPerSecond, st.AvgBlocksPerSecond)
		}
	})

	t.Run("scenario2_sync_from_middle_with_restart", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "scenario2")
		midTarget := earliest + window/2

		first := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18555,
			WSPort:      18556,
			Earliest:    earliest,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})

		waitForCondition(t, 3*time.Minute, func() (bool, error) {
			st, err := first.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.LastSyncedBlock >= midTarget, nil
		}, "scenario2 did not reach 50% backfill before restart")
		first.Stop(t)

		second := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18555,
			WSPort:      18556,
			Earliest:    earliest,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})
		defer second.Stop(t)

		waitForCondition(t, 4*time.Minute, func() (bool, error) {
			st, err := second.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.GapsRemaining == 0 && st.LastSyncedBlock >= head, nil
		}, "scenario2 restart did not refill gaps and catch up")

		st, err := second.Status(context.Background())
		if err == nil {
			t.Logf("scenario2 metrics: head=%d last=%d gaps=%d/%d blocks=%d indexed=%d bps=%.3f avg_bps=%.3f",
				st.ChainHead, st.LastSyncedBlock, st.GapsRemaining, st.GapsTotal,
				st.BlocksProcessed, st.BlocksIndexed, st.BlocksPerSecond, st.AvgBlocksPerSecond)
		}
	})

	t.Run("scenario3_sync_from_latest_then_backfill", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "scenario3")

		latestFirst := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18565,
			WSPort:      18566,
			Earliest:    head,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})

		waitForCondition(t, 2*time.Minute, func() (bool, error) {
			st, err := latestFirst.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.LastSyncedBlock >= head, nil
		}, "scenario3 latest-first phase did not catch up")
		latestFirst.Stop(t)

		backfill := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18565,
			WSPort:      18566,
			Earliest:    earliest,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})
		defer backfill.Stop(t)

		waitForCondition(t, 4*time.Minute, func() (bool, error) {
			st, err := backfill.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.GapsTotal > 0, nil
		}, "scenario3 backfill phase did not report gap segments")

		waitForCondition(t, 4*time.Minute, func() (bool, error) {
			st, err := backfill.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.GapsRemaining == 0 && st.LastSyncedBlock >= head, nil
		}, "scenario3 did not finish backfilling historical gaps")

		st, err := backfill.Status(context.Background())
		if err == nil {
			t.Logf("scenario3 metrics: head=%d last=%d gaps=%d/%d blocks=%d indexed=%d bps=%.3f avg_bps=%.3f",
				st.ChainHead, st.LastSyncedBlock, st.GapsRemaining, st.GapsTotal,
				st.BlocksProcessed, st.BlocksIndexed, st.BlocksPerSecond, st.AvgBlocksPerSecond)
		}
	})

	t.Run("scenario4_and_5_query_while_syncing_and_cache_ratio", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "scenario45")
		proc := startGateway(t, gatewayStartConfig{
			BinaryPath:  bin,
			DataDir:     dataDir,
			RPCPort:     18575,
			WSPort:      18576,
			Earliest:    earliest,
			FetchJobs:   4,
			CometRPC:    cometRPC,
			GRPCAddr:    grpcAddr,
			ChainID:     chainID,
			EnableSync:  true,
			EnableRPC:   true,
			APIList:     "eth,net,web3,inj",
			WaitTimeout: 60 * time.Second,
		})
		defer proc.Stop(t)

		targetBlock := earliest
		if recentTx != nil {
			targetBlock = recentTx.Block
		}
		waitForCondition(t, 4*time.Minute, func() (bool, error) {
			st, err := proc.Status(context.Background())
			if err != nil {
				return false, err
			}
			return st.LastSyncedBlock >= targetBlock, nil
		}, "scenario4 did not reach target sync block")

		if recentTx != nil {
			var pre syncStatusResponse
			waitForCondition(t, 2*time.Minute, func() (bool, error) {
				tx, err := ethTransactionByHash(context.Background(), proc.RPCURL(), recentTx.Hash)
				if err != nil {
					return false, err
				}
				if tx == nil {
					return false, nil
				}
				st, err := proc.Status(context.Background())
				if err != nil {
					return false, err
				}
				pre = st
				return true, nil
			}, "scenario4 transaction lookup did not return during sync")

			for i := 0; i < 8; i++ {
				if _, err := ethTransactionByHash(context.Background(), proc.RPCURL(), recentTx.Hash); err != nil {
					if isUnavailableErr(err) {
						t.Skipf("scenario4/5 requires available query backend, got: %v", err)
					}
					t.Fatalf("query cached tx hash: %v", err)
				}
			}

			post, err := proc.Status(context.Background())
			if err != nil {
				t.Fatalf("read status after cached queries: %v", err)
			}

			if post.Cache.TxByHash.Hits == 0 {
				t.Fatalf("expected tx_by_hash cache hits > 0, got %+v", post.Cache.TxByHash)
			}
			if post.Cache.TxByHash.Hits <= pre.Cache.TxByHash.Hits {
				t.Fatalf("expected cache hits to increase after repeated synced-range queries, before=%d after=%d", pre.Cache.TxByHash.Hits, post.Cache.TxByHash.Hits)
			}

			if oldTx != nil {
				beforeOld := post
				for i := 0; i < 4; i++ {
					if _, err := ethTransactionByHash(context.Background(), proc.RPCURL(), oldTx.Hash); err != nil {
						t.Fatalf("query old tx hash: %v", err)
					}
				}
				afterOld, err := proc.Status(context.Background())
				if err != nil {
					t.Fatalf("read status after old-range queries: %v", err)
				}
				if afterOld.Cache.TxByHash.LiveFallbacks <= beforeOld.Cache.TxByHash.LiveFallbacks {
					t.Fatalf("expected live fallbacks to increase for non-indexed range, before=%d after=%d",
						beforeOld.Cache.TxByHash.LiveFallbacks, afterOld.Cache.TxByHash.LiveFallbacks)
				}
				t.Logf("scenario4/5 metrics (tx mode): tx_by_hash=%+v tx_by_index=%+v receipt_by_hash=%+v block_logs=%+v",
					afterOld.Cache.TxByHash, afterOld.Cache.TxByIndex, afterOld.Cache.ReceiptByHash, afterOld.Cache.BlockLogs)
				return
			}

			if earliest > 1 {
				oldBlock := earliest - 1
				beforeOld := post
				for i := 0; i < 4; i++ {
					if _, err := ethGetLogsForSingleBlock(context.Background(), proc.RPCURL(), oldBlock); err != nil {
						if isUnavailableErr(err) {
							t.Skipf("scenario4/5 requires available query backend for old-range eth_getLogs, got: %v", err)
						}
						t.Fatalf("query non-indexed block logs: %v", err)
					}
				}
				afterOld, err := proc.Status(context.Background())
				if err != nil {
					t.Fatalf("read status after non-indexed log queries: %v", err)
				}
				if afterOld.Cache.BlockLogs.LiveFallbacks <= beforeOld.Cache.BlockLogs.LiveFallbacks {
					t.Fatalf("expected block_logs live fallbacks to increase for non-indexed range, before=%d after=%d",
						beforeOld.Cache.BlockLogs.LiveFallbacks, afterOld.Cache.BlockLogs.LiveFallbacks)
				}
				t.Logf("scenario4/5 metrics (tx+logs mode): tx_by_hash=%+v tx_by_index=%+v receipt_by_hash=%+v block_logs=%+v",
					afterOld.Cache.TxByHash, afterOld.Cache.TxByIndex, afterOld.Cache.ReceiptByHash, afterOld.Cache.BlockLogs)
				return
			}

			t.Logf("scenario4/5 metrics (tx mode, no old range): tx_by_hash=%+v tx_by_index=%+v receipt_by_hash=%+v block_logs=%+v",
				post.Cache.TxByHash, post.Cache.TxByIndex, post.Cache.ReceiptByHash, post.Cache.BlockLogs)
			return
		}

		// Fallback mode for chains with no discoverable recent EVM tx hashes:
		// use eth_getLogs to validate cache hits on indexed range and live fallbacks on non-indexed range.
		indexedBlock := earliest
		if indexedBlock < 1 {
			indexedBlock = 1
		}

		pre, err := proc.Status(context.Background())
		if err != nil {
			t.Fatalf("read status before log queries: %v", err)
		}
		for i := 0; i < 8; i++ {
			if _, err := ethGetLogsForSingleBlock(context.Background(), proc.RPCURL(), indexedBlock); err != nil {
				if isUnavailableErr(err) {
					t.Skipf("scenario4/5 requires available query backend for eth_getLogs, got: %v", err)
				}
				t.Fatalf("query indexed block logs: %v", err)
			}
		}

		post, err := proc.Status(context.Background())
		if err != nil {
			t.Fatalf("read status after indexed log queries: %v", err)
		}
		if post.Cache.BlockLogs.Hits <= pre.Cache.BlockLogs.Hits {
			t.Fatalf("expected block_logs cache hits to increase, before=%d after=%d",
				pre.Cache.BlockLogs.Hits, post.Cache.BlockLogs.Hits)
		}

		if earliest > 1 {
			oldBlock := earliest - 1
			beforeOld := post
			for i := 0; i < 4; i++ {
				if _, err := ethGetLogsForSingleBlock(context.Background(), proc.RPCURL(), oldBlock); err != nil {
					if isUnavailableErr(err) {
						t.Skipf("scenario4/5 requires available query backend for old-range eth_getLogs, got: %v", err)
					}
					t.Fatalf("query non-indexed block logs: %v", err)
				}
			}
			afterOld, err := proc.Status(context.Background())
			if err != nil {
				t.Fatalf("read status after non-indexed log queries: %v", err)
			}
			if afterOld.Cache.BlockLogs.LiveFallbacks <= beforeOld.Cache.BlockLogs.LiveFallbacks {
				t.Fatalf("expected block_logs live fallbacks to increase for non-indexed range, before=%d after=%d",
					beforeOld.Cache.BlockLogs.LiveFallbacks, afterOld.Cache.BlockLogs.LiveFallbacks)
			}
			t.Logf("scenario4/5 metrics (logs mode): tx_by_hash=%+v tx_by_index=%+v receipt_by_hash=%+v block_logs=%+v",
				afterOld.Cache.TxByHash, afterOld.Cache.TxByIndex, afterOld.Cache.ReceiptByHash, afterOld.Cache.BlockLogs)
			return
		}

		t.Logf("scenario4/5 metrics (logs mode, no old range): tx_by_hash=%+v tx_by_index=%+v receipt_by_hash=%+v block_logs=%+v",
			post.Cache.TxByHash, post.Cache.TxByIndex, post.Cache.ReceiptByHash, post.Cache.BlockLogs)
	})
}

type gatewayStartConfig struct {
	BinaryPath     string
	DataDir        string
	RPCPort        int
	WSPort         int
	Earliest       int64
	FetchJobs      int
	CometRPC       string
	GRPCAddr       string
	ChainID        string
	EnableSync     bool
	EnableRPC      bool
	OfflineRPCOnly bool
	APIList        string
	WaitTimeout    time.Duration
}

type gatewayProcess struct {
	cmd       *exec.Cmd
	done      chan error
	logPath   string
	rpcURL    string
	statusURL string
}

func startGateway(t *testing.T, cfg gatewayStartConfig) *gatewayProcess {
	t.Helper()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	if err := requireTCPPortFree(cfg.RPCPort); err != nil {
		t.Fatalf("rpc port preflight failed: %v", err)
	}
	if err := requireTCPPortFree(cfg.WSPort); err != nil {
		t.Fatalf("ws port preflight failed: %v", err)
	}

	logPath := filepath.Join(cfg.DataDir, "gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}

	cmd := exec.Command(cfg.BinaryPath)
	cmd.Env = append(os.Environ(),
		"WEB3INJ_LOG_FORMAT=json",
		"WEB3INJ_LOG_VERBOSE=false",
		fmt.Sprintf("WEB3INJ_DATA_DIR=%s", cfg.DataDir),
		fmt.Sprintf("WEB3INJ_EARLIEST_BLOCK=%d", cfg.Earliest),
		fmt.Sprintf("WEB3INJ_FETCH_JOBS=%d", cfg.FetchJobs),
		fmt.Sprintf("WEB3INJ_COMET_RPC=%s", cfg.CometRPC),
		fmt.Sprintf("WEB3INJ_GRPC_ADDR=%s", cfg.GRPCAddr),
		fmt.Sprintf("WEB3INJ_CHAIN_ID=%s", cfg.ChainID),
		fmt.Sprintf("WEB3INJ_ENABLE_SYNC=%t", cfg.EnableSync),
		fmt.Sprintf("WEB3INJ_JSONRPC_ENABLE=%t", cfg.EnableRPC),
		fmt.Sprintf("WEB3INJ_OFFLINE_RPC_ONLY=%t", cfg.OfflineRPCOnly),
		fmt.Sprintf("WEB3INJ_JSONRPC_ADDRESS=127.0.0.1:%d", cfg.RPCPort),
		fmt.Sprintf("WEB3INJ_JSONRPC_WS_ADDRESS=127.0.0.1:%d", cfg.WSPort),
		fmt.Sprintf("WEB3INJ_JSONRPC_API=%s", cfg.APIList),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	proc := &gatewayProcess{
		cmd:       cmd,
		done:      done,
		logPath:   logPath,
		rpcURL:    fmt.Sprintf("http://127.0.0.1:%d", cfg.RPCPort),
		statusURL: fmt.Sprintf("http://127.0.0.1:%d/status/sync", cfg.RPCPort),
	}

	waitForCondition(t, cfg.WaitTimeout, func() (bool, error) {
		select {
		case err := <-done:
			if err != nil {
				return false, errors.Wrap(err, "gateway exited early")
			}
			return false, errors.New("gateway exited early")
		default:
		}
		_, err := proc.Status(context.Background())
		if err != nil {
			return false, nil
		}
		return true, nil
	}, "gateway status endpoint did not become ready")

	return proc
}

func requireTCPPortFree(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

func (p *gatewayProcess) Stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}

	select {
	case <-p.done:
		return
	default:
	}

	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	case <-p.done:
	}
}

func (p *gatewayProcess) RPCURL() string {
	return p.rpcURL
}

func (p *gatewayProcess) Status(ctx context.Context) (syncStatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.statusURL, nil)
	if err != nil {
		return syncStatusResponse{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return syncStatusResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return syncStatusResponse{}, fmt.Errorf("status code %d: %s", resp.StatusCode, string(b))
	}

	var out syncStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return syncStatusResponse{}, err
	}
	return out, nil
}

type syncStatusResponse struct {
	Threads            int     `json:"threads"`
	Phase              string  `json:"phase"`
	ChainHead          int64   `json:"chain_head"`
	LastSyncedBlock    int64   `json:"last_synced_block"`
	BlocksProcessed    int64   `json:"blocks_processed"`
	BlocksIndexed      int64   `json:"blocks_indexed"`
	BlocksPerSecond    float64 `json:"blocks_per_second"`
	AvgBlocksPerSecond float64 `json:"avg_blocks_per_second"`
	GapsTotal          int     `json:"gaps_total"`
	GapsRemaining      int     `json:"gaps_remaining"`
	Cache              struct {
		TxByHash      cacheLookup `json:"tx_by_hash"`
		TxByIndex     cacheLookup `json:"tx_by_index"`
		ReceiptByHash cacheLookup `json:"receipt_by_hash"`
		BlockLogs     cacheLookup `json:"block_logs"`
	} `json:"cache"`
}

type cacheLookup struct {
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
	LiveFallbacks uint64 `json:"live_fallbacks"`
}

type txCandidate struct {
	Hash  string
	Block int64
}

func discoverTxCandidates(ctx context.Context, rpcURL string, head int64, depth int64) ([]txCandidate, error) {
	var out []txCandidate
	minHeight := maxInt64(1, head-depth)

	seen := make(map[string]struct{})
	for h := head; h >= minHeight; h-- {
		hashes, err := ethBlockTxHashes(ctx, rpcURL, h)
		if err != nil {
			return nil, err
		}
		for _, hash := range hashes {
			if _, ok := seen[hash]; ok {
				continue
			}
			seen[hash] = struct{}{}
			tx, err := ethTransactionByHash(ctx, rpcURL, hash)
			if err != nil || tx == nil {
				continue
			}
			block, err := hexToInt64(tx.BlockNumber)
			if err != nil {
				continue
			}
			out = append(out, txCandidate{Hash: hash, Block: block})
			if len(out) >= 50 {
				return out, nil
			}
		}
	}

	return out, nil
}

func pickTxCandidates(candidates []txCandidate, earliest int64) (recent *txCandidate, old *txCandidate) {
	for i := range candidates {
		c := candidates[i]
		if recent == nil && c.Block >= earliest {
			cp := c
			recent = &cp
		}
		if old == nil && c.Block < earliest {
			cp := c
			old = &cp
		}
		if recent != nil && old != nil {
			return recent, old
		}
	}
	return recent, old
}

type rpcEnvelope struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func ethBlockNumber(ctx context.Context, rpcURL string) (int64, error) {
	var hexHeight string
	if err := rpcCall(ctx, rpcURL, "eth_blockNumber", []interface{}{}, &hexHeight); err != nil {
		return 0, err
	}
	return hexToInt64(hexHeight)
}

func web3ClientVersion(ctx context.Context, rpcURL string) (string, error) {
	var out string
	if err := rpcCall(ctx, rpcURL, "web3_clientVersion", []interface{}{}, &out); err != nil {
		return "", err
	}
	return out, nil
}

func ethBlockTxHashes(ctx context.Context, rpcURL string, height int64) ([]string, error) {
	blockTag := fmt.Sprintf("0x%x", height)
	var block struct {
		Transactions []string `json:"transactions"`
	}
	if err := rpcCall(ctx, rpcURL, "eth_getBlockByNumber", []interface{}{blockTag, false}, &block); err != nil {
		if errors.Is(err, errRPCNullResult) {
			return nil, nil
		}
		return nil, err
	}
	return block.Transactions, nil
}

type ethTx struct {
	Hash        string `json:"hash"`
	BlockNumber string `json:"blockNumber"`
}

func ethTransactionByHash(ctx context.Context, rpcURL, hash string) (*ethTx, error) {
	var tx ethTx
	if err := rpcCall(ctx, rpcURL, "eth_getTransactionByHash", []interface{}{hash}, &tx); err != nil {
		if errors.Is(err, errRPCNullResult) {
			return nil, nil
		}
		return nil, err
	}
	return &tx, nil
}

func ethGetLogsForSingleBlock(ctx context.Context, rpcURL string, height int64) ([]json.RawMessage, error) {
	blockTag := fmt.Sprintf("0x%x", height)
	filter := map[string]interface{}{
		"fromBlock": blockTag,
		"toBlock":   blockTag,
	}
	var logs []json.RawMessage
	if err := rpcCall(ctx, rpcURL, "eth_getLogs", []interface{}{filter}, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

func isUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "code = Unavailable") || strings.Contains(msg, "connection refused")
}

var errRPCNullResult = errors.New("rpc null result")

func rpcCall(ctx context.Context, rpcURL, method string, params interface{}, out interface{}) error {
	reqBody, err := json.Marshal(rpcEnvelope{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rpc http status %d: %s", resp.StatusCode, string(b))
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return err
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if strings.TrimSpace(string(rpcResp.Result)) == "null" {
		return errRPCNullResult
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rpcResp.Result, out)
}

func buildGatewayBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "evm-gateway-e2e")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/evm-gateway")
	cmd.Dir = projectRoot(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, string(output))
	}
	return binPath
}

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, ".."))
}

func waitForCondition(t *testing.T, timeout time.Duration, check func() (bool, error), failure string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := check()
		if err == nil && ok {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatal(failure)
}

func hexToInt64(v string) (int64, error) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "0x")
	if v == "" {
		return 0, fmt.Errorf("empty hex value")
	}
	n, err := strconv.ParseInt(v, 16, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func getenvDurationSeconds(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func cometLatestHeight(ctx context.Context, cometRPC string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cometRPC, "/")+"/status", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var payload struct {
		Result struct {
			SyncInfo struct {
				LatestBlockHeight string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	return strconv.ParseInt(payload.Result.SyncInfo.LatestBlockHeight, 10, 64)
}

func cometChainID(ctx context.Context, cometRPC string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cometRPC, "/")+"/status", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var payload struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Result.NodeInfo.Network) == "" {
		return "", fmt.Errorf("empty chain id from comet status")
	}
	return payload.Result.NodeInfo.Network, nil
}
