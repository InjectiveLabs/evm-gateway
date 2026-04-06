package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	defaultBenchGatewayRPCPort           = 8746
	defaultBenchGatewayWSPort            = 8747
	defaultBenchSeedDuration             = 20 * time.Second
	defaultBenchWarmupDuration           = 15 * time.Second
	defaultBenchMeasureDuration          = 4 * time.Minute
	defaultBenchSeedSettleDelay          = 3 * time.Second
	defaultBenchRequestTimeout           = 20 * time.Second
	defaultBenchFetchJobs                = 8
	defaultBenchSeedAccountsNum          = 64
	defaultBenchInternalCallIters        = 300
	defaultBenchTxCandidateLimit         = 256
	defaultBenchBucketDuration           = 5 * time.Second
	defaultBenchMaxLogsPerRange          = 2000
	defaultBenchMinNoFile         uint64 = 8192
)

//go:embed benchmark_report_template.html
var benchmarkReportHTML string

func TestHistoricalRPCBenchmarkSuite(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E_BENCH") != "1" {
		t.Skip("set WEB3INJ_E2E_BENCH=1 to run historical rpc benchmark e2e")
	}

	ctx := context.Background()
	cfg := loadBenchmarkConfig(t)
	if cfg.OfflineAfterSync {
		t.Log("offline-after-sync mode enabled; measured phase will restart the gateway in KV-only rpc mode")
	}
	limit := ensureOpenFileLimit(cfg.MinNoFile)
	if strings.TrimSpace(limit.Warning) != "" {
		t.Logf("nofile warning: %s", limit.Warning)
	}

	artifactsDir := prepareBenchmarkOutputDir(t)
	t.Logf("benchmark artifacts dir: %s", artifactsDir)

	chainID, err := cometChainID(ctx, cfg.CometRPC)
	if err != nil {
		t.Fatalf("query comet chain id: %v", err)
	}
	cfg.ChainID = chainID

	stresserRoot := getenv(
		"WEB3INJ_E2E_CHAIN_STRESSER_DIR",
		filepath.Clean(filepath.Join(projectRoot(t), "..", "chain-stresser")),
	)
	accountsPath := filepath.Join(stresserRoot, "chain-stresser-deploy", "instances", "0", "accounts.json")
	if _, err := os.Stat(accountsPath); err != nil {
		t.Fatalf("accounts file not found at %s: %v", accountsPath, err)
	}

	headBefore, err := ethBlockNumber(ctx, cfg.SourceRPC)
	if err != nil {
		t.Fatalf("query source head before seeding: %v", err)
	}

	stresserBin := buildChainStresserBinary(t, stresserRoot)
	seedWorkloads := []benchmarkSeedWorkload{
		{Name: "eth_send", Command: "tx-eth-send", Args: []string{"tx-eth-send"}},
		{Name: "eth_call", Command: "tx-eth-call", Args: []string{"tx-eth-call"}},
		{Name: "eth_deploy", Command: "tx-eth-deploy", Args: []string{"tx-eth-deploy"}},
		{
			Name:    "eth_internal_call",
			Command: "tx-eth-internal-call",
			Args:    []string{"tx-eth-internal-call", "--iterations", strconv.Itoa(cfg.InternalCallIterations)},
		},
	}
	commonSeedArgs := []string{
		"--chain-id", cfg.ChainID,
		"--eth-chain-id", strconv.FormatInt(cfg.EthChainID, 10),
		"--node-addr", endpointHostPort(t, cfg.CometRPC),
		"--grpc-addr", cfg.GRPCAddr,
		"--accounts", accountsPath,
		"--accounts-num", strconv.Itoa(cfg.SeedAccountsNum),
	}

	for i := range seedWorkloads {
		workload := &seedWorkloads[i]
		t.Run("seed_"+workload.Name, func(t *testing.T) {
			start := time.Now()
			runChainStresserFor(
				t,
				stresserRoot,
				stresserBin,
				append(append([]string{}, workload.Args...), commonSeedArgs...),
				cfg.SeedDuration,
			)
			workload.DurationSeconds = roundSeconds(time.Since(start))
		})
	}

	time.Sleep(cfg.SeedSettleDelay)

	headAfter, err := ethBlockNumber(ctx, cfg.SourceRPC)
	if err != nil {
		t.Fatalf("query source head after seeding: %v", err)
	}
	if headAfter <= headBefore {
		t.Fatalf("seeding produced no new blocks: before=%d after=%d", headBefore, headAfter)
	}

	generatedFrom := maxInt64(1, headBefore+1)
	txs, err := discoverTxCandidatesInRange(ctx, cfg.SourceRPC, generatedFrom, headAfter, cfg.TxCandidateLimit)
	if err != nil {
		t.Fatalf("discover tx candidates in seeded range: %v", err)
	}
	if len(txs) < 16 {
		t.Fatalf("need at least 16 tx candidates in seeded range [%d,%d], got %d", generatedFrom, headAfter, len(txs))
	}

	fixtures := prepareBenchmarkFixtures(t, ctx, cfg.SourceRPC, generatedFrom, headAfter, txs)

	gatewayBin := buildGatewayBinary(t)
	gatewayDataDir := filepath.Join(artifactsDir, "gateway")
	proc := startGateway(t, gatewayStartConfig{
		BinaryPath:  gatewayBin,
		DataDir:     gatewayDataDir,
		RPCPort:     cfg.GatewayRPCPort,
		WSPort:      cfg.GatewayWSPort,
		Earliest:    generatedFrom,
		FetchJobs:   cfg.FetchJobs,
		CometRPC:    cfg.CometRPC,
		GRPCAddr:    cfg.GRPCAddr,
		ChainID:     cfg.ChainID,
		EnableSync:  true,
		EnableRPC:   true,
		APIList:     "eth,net,web3,debug,inj",
		WaitTimeout: 90 * time.Second,
	})
	defer func() {
		proc.Stop(t)
	}()

	waitForCondition(t, 5*time.Minute, func() (bool, error) {
		st, err := proc.Status(ctx)
		if err != nil {
			return false, err
		}
		srcHead, err := ethBlockNumber(ctx, cfg.SourceRPC)
		if err != nil {
			return false, err
		}
		dstHead, err := ethBlockNumber(ctx, proc.RPCURL())
		if err != nil {
			return false, err
		}
		return st.GapsRemaining == 0 && dstHead == srcHead && dstHead >= headAfter, nil
	}, "benchmark gateway did not fully sync to source head")
	if cfg.PostSyncSettle > 0 {
		t.Logf("post-sync settle for %s", cfg.PostSyncSettle)
		time.Sleep(cfg.PostSyncSettle)
	}
	if cfg.OfflineAfterSync {
		t.Log("restarting benchmark gateway in offline rpc-only mode against the indexed data dir")
		proc.Stop(t)
		proc = startGateway(t, gatewayStartConfig{
			BinaryPath:     gatewayBin,
			DataDir:        gatewayDataDir,
			RPCPort:        cfg.GatewayRPCPort,
			WSPort:         cfg.GatewayWSPort,
			Earliest:       generatedFrom,
			FetchJobs:      cfg.FetchJobs,
			CometRPC:       cfg.CometRPC,
			GRPCAddr:       cfg.GRPCAddr,
			ChainID:        cfg.ChainID,
			EnableSync:     false,
			EnableRPC:      true,
			OfflineRPCOnly: true,
			APIList:        "eth",
			WaitTimeout:    30 * time.Second,
		})
	}

	scenarios := buildBenchmarkScenarios(cfg, fixtures)
	client, closeClient := newBenchmarkHTTPClient(totalScenarioWorkers(scenarios), cfg.RequestTimeout)
	defer closeClient()

	t.Logf("warming cache for %s with %d scenario workers", cfg.WarmupDuration, totalScenarioWorkers(scenarios))
	runBenchmarkPhase(t, client, proc.RPCURL(), scenarios, cfg.WarmupDuration, cfg.BucketDuration, false)

	statusBefore, err := proc.Status(ctx)
	if err != nil {
		t.Fatalf("query pre-benchmark status: %v", err)
	}

	t.Logf("measuring mixed historical rpc load for %s", cfg.MeasureDuration)
	scenarioReports := runBenchmarkPhase(t, client, proc.RPCURL(), scenarios, cfg.MeasureDuration, cfg.BucketDuration, true)

	statusAfter, err := proc.Status(ctx)
	if err != nil {
		t.Fatalf("query post-benchmark status: %v", err)
	}

	if statusAfter.Cache.TxByHash.LiveFallbacks != statusBefore.Cache.TxByHash.LiveFallbacks ||
		statusAfter.Cache.TxByIndex.LiveFallbacks != statusBefore.Cache.TxByIndex.LiveFallbacks ||
		statusAfter.Cache.ReceiptByHash.LiveFallbacks != statusBefore.Cache.ReceiptByHash.LiveFallbacks ||
		statusAfter.Cache.BlockLogs.LiveFallbacks != statusBefore.Cache.BlockLogs.LiveFallbacks {
		if cfg.StrictCacheOnly {
			t.Fatalf(
				"strict-cache benchmark observed live fallbacks\nbefore=%+v\nafter=%+v",
				statusBefore.Cache,
				statusAfter.Cache,
			)
		}
		t.Logf(
			"cache live fallbacks changed during benchmark\nbefore=%+v\nafter=%+v",
			statusBefore.Cache,
			statusAfter.Cache,
		)
	}

	report := benchmarkReport{
		Suite:                  "historical-rpc-benchmark",
		GeneratedAt:            time.Now().UTC().Format(time.RFC3339),
		OutputDir:              artifactsDir,
		BucketDurationSeconds:  roundSeconds(cfg.BucketDuration),
		WarmupDurationSeconds:  roundSeconds(cfg.WarmupDuration),
		MeasureDurationSeconds: roundSeconds(cfg.MeasureDuration),
		Environment: benchmarkEnvironmentReport{
			SourceRPC:        cfg.SourceRPC,
			CometRPC:         cfg.CometRPC,
			GRPCAddr:         cfg.GRPCAddr,
			ChainID:          cfg.ChainID,
			EthChainID:       cfg.EthChainID,
			OfflineAfterSync: cfg.OfflineAfterSync,
			NoFile:           limit,
		},
		Seed: benchmarkSeedReport{
			DurationSecondsPerWorkload: roundSeconds(cfg.SeedDuration),
			SettleDelaySeconds:         roundSeconds(cfg.SeedSettleDelay),
			AccountsNum:                cfg.SeedAccountsNum,
			InternalCallIterations:     cfg.InternalCallIterations,
			HeadBefore:                 headBefore,
			HeadAfter:                  headAfter,
			GeneratedFrom:              generatedFrom,
			GeneratedTo:                headAfter,
			Workloads:                  seedWorkloads,
		},
		Fixtures: benchmarkFixtureReport{
			HeaderBlocks: len(fixtures.HeaderBlocks),
			FullBlocks:   len(fixtures.FullBlocks),
			RangeFilters: len(fixtures.RangeFilters),
			Transactions: len(fixtures.TxLookups),
			Batches:      len(fixtures.BatchRequests),
			TraceHashes:  len(fixtures.TraceHashes),
		},
		Gateway: benchmarkGatewayReport{
			RPCURL:       proc.RPCURL(),
			DataDir:      gatewayDataDir,
			LogPath:      proc.logPath,
			StatusBefore: statusBefore,
			StatusAfter:  statusAfter,
		},
		Scenarios: scenarioReports,
		Totals:    summarizeScenarioReports(scenarioReports),
	}

	writeBenchmarkArtifacts(t, artifactsDir, report)
	if failures := benchmarkScenarioFailures(scenarioReports); len(failures) > 0 {
		t.Fatalf(
			"benchmark recorded rpc errors in measured window: %s (see %s/report.json)",
			strings.Join(failures, ", "),
			artifactsDir,
		)
	}
	t.Logf("historical benchmark report written to %s", artifactsDir)
}

type benchmarkConfig struct {
	SourceRPC              string
	CometRPC               string
	GRPCAddr               string
	ChainID                string
	EthChainID             int64
	GatewayRPCPort         int
	GatewayWSPort          int
	FetchJobs              int
	SeedDuration           time.Duration
	SeedSettleDelay        time.Duration
	WarmupDuration         time.Duration
	MeasureDuration        time.Duration
	RequestTimeout         time.Duration
	BucketDuration         time.Duration
	PostSyncSettle         time.Duration
	SeedAccountsNum        int
	InternalCallIterations int
	TxCandidateLimit       int
	WorkerScale            int
	MinNoFile              uint64
	StrictCacheOnly        bool
	OfflineAfterSync       bool
}

func loadBenchmarkConfig(t *testing.T) benchmarkConfig {
	t.Helper()

	sourceRPC := getenv("WEB3INJ_E2E_SOURCE_RPC", defaultSourceRPC)
	cometRPC := getenv("WEB3INJ_COMET_RPC", defaultCometRPC)
	grpcAddr := resolveParityGRPCAddr(t)
	offlineAfterSync := strings.TrimSpace(os.Getenv("WEB3INJ_BENCH_OFFLINE_AFTER_SYNC")) == "1"
	strictCacheOnly := strings.TrimSpace(os.Getenv("WEB3INJ_BENCH_STRICT_CACHE_ONLY")) == "1"
	if offlineAfterSync {
		strictCacheOnly = true
	}
	ethChainID, err := ethChainID(context.Background(), sourceRPC)
	if err != nil {
		t.Fatalf("query eth chain id: %v", err)
	}

	return benchmarkConfig{
		SourceRPC:              sourceRPC,
		CometRPC:               cometRPC,
		GRPCAddr:               grpcAddr,
		EthChainID:             ethChainID,
		GatewayRPCPort:         getenvInt("WEB3INJ_BENCH_GATEWAY_RPC_PORT", defaultBenchGatewayRPCPort),
		GatewayWSPort:          getenvInt("WEB3INJ_BENCH_GATEWAY_WS_PORT", defaultBenchGatewayWSPort),
		FetchJobs:              getenvInt("WEB3INJ_BENCH_FETCH_JOBS", defaultBenchFetchJobs),
		SeedDuration:           getenvDurationSeconds("WEB3INJ_BENCH_SEED_DURATION_SEC", defaultBenchSeedDuration),
		SeedSettleDelay:        getenvDurationSeconds("WEB3INJ_BENCH_SEED_SETTLE_SEC", defaultBenchSeedSettleDelay),
		WarmupDuration:         getenvDurationSeconds("WEB3INJ_BENCH_WARMUP_SEC", defaultBenchWarmupDuration),
		MeasureDuration:        getenvDurationSeconds("WEB3INJ_BENCH_DURATION_SEC", defaultBenchMeasureDuration),
		RequestTimeout:         getenvDurationSeconds("WEB3INJ_BENCH_REQUEST_TIMEOUT_SEC", defaultBenchRequestTimeout),
		BucketDuration:         getenvDurationSeconds("WEB3INJ_BENCH_BUCKET_SEC", defaultBenchBucketDuration),
		PostSyncSettle:         getenvDurationSeconds("WEB3INJ_BENCH_POST_SYNC_SETTLE_SEC", 10*time.Second),
		SeedAccountsNum:        getenvInt("WEB3INJ_BENCH_SEED_ACCOUNTS_NUM", defaultBenchSeedAccountsNum),
		InternalCallIterations: getenvInt("WEB3INJ_BENCH_INTERNAL_CALL_ITERATIONS", defaultBenchInternalCallIters),
		TxCandidateLimit:       getenvInt("WEB3INJ_BENCH_TX_CANDIDATE_LIMIT", defaultBenchTxCandidateLimit),
		WorkerScale:            getenvInt("WEB3INJ_BENCH_WORKER_SCALE", 1),
		MinNoFile:              getenvUint64("WEB3INJ_BENCH_MIN_NOFILE", defaultBenchMinNoFile),
		StrictCacheOnly:        strictCacheOnly,
		OfflineAfterSync:       offlineAfterSync,
	}
}

type benchmarkFixtures struct {
	HeaderBlocks  []benchmarkBlockLookup
	FullBlocks    []benchmarkBlockLookup
	RangeFilters  []map[string]interface{}
	TxLookups     []benchmarkTxLookup
	BatchRequests [][]rpcEnvelope
	TraceHashes   []string
}

type benchmarkBlockLookup struct {
	NumberTag string
	Hash      string
}

type benchmarkTxLookup struct {
	Hash             string
	BlockHash        string
	BlockNumber      string
	TransactionIndex string
}

func prepareBenchmarkFixtures(
	t *testing.T,
	ctx context.Context,
	sourceRPC string,
	generatedFrom, headAfter int64,
	candidates []txCandidate,
) benchmarkFixtures {
	t.Helper()

	headerBlocks := buildBenchmarkBlockLookups(t, ctx, sourceRPC, sampleHeights(generatedFrom, headAfter, 192))

	txLookups := make([]benchmarkTxLookup, 0, 96)
	traceHashes := make([]string, 0, 64)
	var txBlocks []int64
	for _, candidate := range sampleTxCandidates(candidates, 96) {
		tx := mustDetailedTx(t, ctx, sourceRPC, candidate.Hash)
		if strings.TrimSpace(tx.BlockHash) == "" || strings.TrimSpace(tx.TransactionIndex) == "" {
			continue
		}
		txLookups = append(txLookups, benchmarkTxLookup{
			Hash:             tx.Hash,
			BlockHash:        tx.BlockHash,
			BlockNumber:      tx.BlockNumber,
			TransactionIndex: tx.TransactionIndex,
		})
		traceHashes = append(traceHashes, tx.Hash)
		txBlocks = append(txBlocks, candidate.Block)
	}
	if len(txLookups) == 0 {
		t.Fatal("unable to prepare benchmark tx lookups from seeded traffic")
	}

	fullBlockHeights := sampleInt64s(uniqueInt64s(txBlocks), 64)
	if len(fullBlockHeights) == 0 {
		fullBlockHeights = sampleHeights(generatedFrom, headAfter, 64)
	}
	fullBlocks := buildBenchmarkBlockLookups(t, ctx, sourceRPC, fullBlockHeights)

	rangeFilters := make([]map[string]interface{}, 0, 96)
	rangeHeights := sampleHeights(generatedFrom, headAfter, 96)
	for idx, start := range rangeHeights {
		filter, ok := selectSafeRangeFilter(ctx, sourceRPC, start, headAfter, idx)
		if ok {
			rangeFilters = append(rangeFilters, filter)
		}
	}
	if len(rangeFilters) == 0 {
		t.Fatal("unable to build safe historical eth_getLogs range filters for benchmark")
	}

	fixtures := benchmarkFixtures{
		HeaderBlocks: headerBlocks,
		FullBlocks:   fullBlocks,
		RangeFilters: rangeFilters,
		TxLookups:    txLookups,
		TraceHashes:  sampleStrings(uniqueStrings(traceHashes), 64),
	}
	fixtures.BatchRequests = buildBenchmarkBatches(fixtures)
	return fixtures
}

func buildBenchmarkBlockLookups(t *testing.T, ctx context.Context, sourceRPC string, heights []int64) []benchmarkBlockLookup {
	t.Helper()

	lookups := make([]benchmarkBlockLookup, 0, len(heights))
	for _, height := range heights {
		tag := fmt.Sprintf("0x%x", height)

		var block parityBlock
		if err := rpcCall(ctx, sourceRPC, "eth_getBlockByNumber", []interface{}{tag, false}, &block); err != nil {
			t.Fatalf("fetch benchmark block %d: %v", height, err)
		}
		if strings.TrimSpace(block.Hash) == "" {
			continue
		}

		lookups = append(lookups, benchmarkBlockLookup{
			NumberTag: tag,
			Hash:      block.Hash,
		})
	}
	if len(lookups) == 0 {
		t.Fatal("unable to prepare benchmark block lookups from seeded traffic")
	}
	return lookups
}

func selectSafeRangeFilter(ctx context.Context, sourceRPC string, start, headAfter int64, ordinal int) (map[string]interface{}, bool) {
	widthCandidates := []int64{8, 4, 2, 1}
	if ordinal%3 == 1 {
		widthCandidates = []int64{4, 2, 1}
	}
	if ordinal%3 == 2 {
		widthCandidates = []int64{2, 1}
	}

	for _, width := range widthCandidates {
		end := start + width - 1
		if end > headAfter {
			end = headAfter
		}

		filter := map[string]interface{}{
			"fromBlock": fmt.Sprintf("0x%x", start),
			"toBlock":   fmt.Sprintf("0x%x", end),
		}

		var logs []json.RawMessage
		err := rpcCall(ctx, sourceRPC, "eth_getLogs", []interface{}{filter}, &logs)
		if err == nil && len(logs) <= defaultBenchMaxLogsPerRange {
			return filter, true
		}
		if err == nil && len(logs) > defaultBenchMaxLogsPerRange {
			continue
		}
		if err != nil && strings.Contains(err.Error(), "more than 10000 results") {
			continue
		}
	}

	return nil, false
}

func buildBenchmarkBatches(fixtures benchmarkFixtures) [][]rpcEnvelope {
	count := minInt(64, len(fixtures.TxLookups))
	if count == 0 {
		return nil
	}

	batches := make([][]rpcEnvelope, 0, count)
	for i := 0; i < count; i++ {
		headerBlock := fixtures.HeaderBlocks[i%len(fixtures.HeaderBlocks)]
		fullBlock := fixtures.FullBlocks[i%len(fixtures.FullBlocks)]
		tx := fixtures.TxLookups[i%len(fixtures.TxLookups)]
		filter := fixtures.RangeFilters[i%len(fixtures.RangeFilters)]
		batches = append(batches, []rpcEnvelope{
			{JSONRPC: "2.0", ID: 1000 + i*10 + 1, Method: "eth_chainId", Params: []interface{}{}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 2, Method: "eth_getBlockByNumber", Params: []interface{}{headerBlock.NumberTag, false}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 3, Method: "eth_getBlockByHash", Params: []interface{}{headerBlock.Hash, false}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 4, Method: "eth_getBlockByNumber", Params: []interface{}{fullBlock.NumberTag, true}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 5, Method: "eth_getBlockByHash", Params: []interface{}{fullBlock.Hash, true}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 6, Method: "eth_getTransactionByHash", Params: []interface{}{tx.Hash}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 7, Method: "eth_getTransactionReceipt", Params: []interface{}{tx.Hash}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 8, Method: "eth_getTransactionByBlockNumberAndIndex", Params: []interface{}{tx.BlockNumber, tx.TransactionIndex}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 9, Method: "eth_getTransactionByBlockHashAndIndex", Params: []interface{}{tx.BlockHash, tx.TransactionIndex}},
			{JSONRPC: "2.0", ID: 1000 + i*10 + 10, Method: "eth_getLogs", Params: []interface{}{filter}},
		})
	}
	return batches
}

type benchmarkScenarioSpec struct {
	Name        string
	Signature   string
	Workers     int
	Timeout     time.Duration
	Invocations []benchmarkInvocation
}

type benchmarkInvocation struct {
	Method string
	Params interface{}
	Batch  []rpcEnvelope
}

func (inv benchmarkInvocation) Execute(ctx context.Context, client *http.Client, rpcURL string) error {
	if len(inv.Batch) > 0 {
		responses, err := rpcBatchCallWithClient(ctx, client, rpcURL, inv.Batch)
		if err != nil {
			return err
		}
		for _, resp := range responses {
			if resp.Error != nil {
				return fmt.Errorf("batch rpc error id=%d code=%d: %s", resp.ID, resp.Error.Code, resp.Error.Message)
			}
		}
		return nil
	}

	resp, err := rpcCallResponseWithClient(ctx, client, rpcURL, inv.Method, inv.Params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

func buildBenchmarkScenarios(cfg benchmarkConfig, fixtures benchmarkFixtures) []benchmarkScenarioSpec {
	scaled := func(base int) int {
		return maxInt(1, base*cfg.WorkerScale)
	}

	headerInvocations := make([]benchmarkInvocation, 0, len(fixtures.HeaderBlocks))
	headerByHashInvocations := make([]benchmarkInvocation, 0, len(fixtures.HeaderBlocks))
	for _, block := range fixtures.HeaderBlocks {
		headerInvocations = append(headerInvocations, benchmarkInvocation{
			Method: "eth_getBlockByNumber",
			Params: []interface{}{block.NumberTag, false},
		})
		headerByHashInvocations = append(headerByHashInvocations, benchmarkInvocation{
			Method: "eth_getBlockByHash",
			Params: []interface{}{block.Hash, false},
		})
	}

	fullBlockInvocations := make([]benchmarkInvocation, 0, len(fixtures.FullBlocks))
	fullBlockByHashInvocations := make([]benchmarkInvocation, 0, len(fixtures.FullBlocks))
	for _, block := range fixtures.FullBlocks {
		fullBlockInvocations = append(fullBlockInvocations, benchmarkInvocation{
			Method: "eth_getBlockByNumber",
			Params: []interface{}{block.NumberTag, true},
		})
		fullBlockByHashInvocations = append(fullBlockByHashInvocations, benchmarkInvocation{
			Method: "eth_getBlockByHash",
			Params: []interface{}{block.Hash, true},
		})
	}

	rangeInvocations := make([]benchmarkInvocation, 0, len(fixtures.RangeFilters))
	for _, filter := range fixtures.RangeFilters {
		rangeInvocations = append(rangeInvocations, benchmarkInvocation{
			Method: "eth_getLogs",
			Params: []interface{}{filter},
		})
	}

	txByHashInvocations := make([]benchmarkInvocation, 0, len(fixtures.TxLookups))
	receiptInvocations := make([]benchmarkInvocation, 0, len(fixtures.TxLookups))
	txByBlockNumberInvocations := make([]benchmarkInvocation, 0, len(fixtures.TxLookups))
	txByBlockHashInvocations := make([]benchmarkInvocation, 0, len(fixtures.TxLookups))
	for _, tx := range fixtures.TxLookups {
		txByHashInvocations = append(txByHashInvocations, benchmarkInvocation{
			Method: "eth_getTransactionByHash",
			Params: []interface{}{tx.Hash},
		})
		receiptInvocations = append(receiptInvocations, benchmarkInvocation{
			Method: "eth_getTransactionReceipt",
			Params: []interface{}{tx.Hash},
		})
		txByBlockNumberInvocations = append(txByBlockNumberInvocations, benchmarkInvocation{
			Method: "eth_getTransactionByBlockNumberAndIndex",
			Params: []interface{}{tx.BlockNumber, tx.TransactionIndex},
		})
		txByBlockHashInvocations = append(txByBlockHashInvocations, benchmarkInvocation{
			Method: "eth_getTransactionByBlockHashAndIndex",
			Params: []interface{}{tx.BlockHash, tx.TransactionIndex},
		})
	}

	batchInvocations := make([]benchmarkInvocation, 0, len(fixtures.BatchRequests))
	for _, batch := range fixtures.BatchRequests {
		batchInvocations = append(batchInvocations, benchmarkInvocation{
			Batch: batch,
		})
	}

	traceInvocations := make([]benchmarkInvocation, 0, len(fixtures.TraceHashes))
	for _, hash := range fixtures.TraceHashes {
		traceInvocations = append(traceInvocations, benchmarkInvocation{
			Method: "debug_traceTransaction",
			Params: []interface{}{hash, map[string]interface{}{"tracer": "callTracer"}},
		})
	}

	all := []benchmarkScenarioSpec{
		{
			Name:        "eth_getBlockByNumber_false",
			Signature:   benchmarkInvocationSignature(headerInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: headerInvocations,
		},
		{
			Name:        "eth_getBlockByNumber_true",
			Signature:   benchmarkInvocationSignature(fullBlockInvocations[0]),
			Workers:     scaled(2),
			Timeout:     12 * time.Second,
			Invocations: fullBlockInvocations,
		},
		{
			Name:        "eth_getBlockByHash_false",
			Signature:   benchmarkInvocationSignature(headerByHashInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: headerByHashInvocations,
		},
		{
			Name:        "eth_getBlockByHash_true",
			Signature:   benchmarkInvocationSignature(fullBlockByHashInvocations[0]),
			Workers:     scaled(2),
			Timeout:     12 * time.Second,
			Invocations: fullBlockByHashInvocations,
		},
		{
			Name:        "eth_getLogs",
			Signature:   benchmarkInvocationSignature(rangeInvocations[0]),
			Workers:     scaled(3),
			Timeout:     12 * time.Second,
			Invocations: rangeInvocations,
		},
		{
			Name:        "eth_getTransactionByHash",
			Signature:   benchmarkInvocationSignature(txByHashInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: txByHashInvocations,
		},
		{
			Name:        "eth_getTransactionReceipt",
			Signature:   benchmarkInvocationSignature(receiptInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: receiptInvocations,
		},
		{
			Name:        "eth_getTransactionByBlockNumberAndIndex",
			Signature:   benchmarkInvocationSignature(txByBlockNumberInvocations[0]),
			Workers:     scaled(2),
			Timeout:     10 * time.Second,
			Invocations: txByBlockNumberInvocations,
		},
		{
			Name:        "eth_getTransactionByBlockHashAndIndex",
			Signature:   benchmarkInvocationSignature(txByBlockHashInvocations[0]),
			Workers:     scaled(2),
			Timeout:     10 * time.Second,
			Invocations: txByBlockHashInvocations,
		},
		{
			Name:        "batch_mixed",
			Signature:   benchmarkInvocationSignature(batchInvocations[0]),
			Workers:     scaled(4),
			Timeout:     15 * time.Second,
			Invocations: batchInvocations,
		},
		{
			Name:        "debug_traceTransaction",
			Signature:   benchmarkInvocationSignature(traceInvocations[0]),
			Workers:     scaled(4),
			Timeout:     20 * time.Second,
			Invocations: traceInvocations,
		},
	}

	if !cfg.StrictCacheOnly {
		return all
	}

	return []benchmarkScenarioSpec{
		{
			Name:        "eth_getBlockByNumber_false",
			Signature:   benchmarkInvocationSignature(headerInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: headerInvocations,
		},
		{
			Name:        "eth_getBlockByNumber_true",
			Signature:   benchmarkInvocationSignature(fullBlockInvocations[0]),
			Workers:     scaled(2),
			Timeout:     12 * time.Second,
			Invocations: fullBlockInvocations,
		},
		{
			Name:        "eth_getBlockByHash_false",
			Signature:   benchmarkInvocationSignature(headerByHashInvocations[0]),
			Workers:     scaled(3),
			Timeout:     10 * time.Second,
			Invocations: headerByHashInvocations,
		},
		{
			Name:        "eth_getBlockByHash_true",
			Signature:   benchmarkInvocationSignature(fullBlockByHashInvocations[0]),
			Workers:     scaled(2),
			Timeout:     12 * time.Second,
			Invocations: fullBlockByHashInvocations,
		},
		{
			Name:        "eth_getLogs",
			Signature:   benchmarkInvocationSignature(rangeInvocations[0]),
			Workers:     scaled(3),
			Timeout:     12 * time.Second,
			Invocations: rangeInvocations,
		},
		{
			Name:        "eth_getTransactionByHash",
			Signature:   benchmarkInvocationSignature(txByHashInvocations[0]),
			Workers:     scaled(12),
			Timeout:     10 * time.Second,
			Invocations: txByHashInvocations,
		},
		{
			Name:        "eth_getTransactionReceipt",
			Signature:   benchmarkInvocationSignature(receiptInvocations[0]),
			Workers:     scaled(8),
			Timeout:     10 * time.Second,
			Invocations: receiptInvocations,
		},
		{
			Name:        "eth_getTransactionByBlockNumberAndIndex",
			Signature:   benchmarkInvocationSignature(txByBlockNumberInvocations[0]),
			Workers:     scaled(4),
			Timeout:     10 * time.Second,
			Invocations: txByBlockNumberInvocations,
		},
		{
			Name:        "eth_getTransactionByBlockHashAndIndex",
			Signature:   benchmarkInvocationSignature(txByBlockHashInvocations[0]),
			Workers:     scaled(4),
			Timeout:     10 * time.Second,
			Invocations: txByBlockHashInvocations,
		},
		{
			Name:        "batch_mixed",
			Signature:   benchmarkInvocationSignature(batchInvocations[0]),
			Workers:     scaled(4),
			Timeout:     15 * time.Second,
			Invocations: batchInvocations,
		},
	}
}

func benchmarkInvocationSignature(inv benchmarkInvocation) string {
	if len(inv.Batch) > 0 {
		bz, err := json.Marshal(inv.Batch)
		if err != nil {
			return "batch(...)"
		}
		return "batch(" + string(bz) + ")"
	}
	bz, err := json.Marshal(inv.Params)
	if err != nil {
		return inv.Method + "(...)"
	}
	return inv.Method + "(" + string(bz) + ")"
}

func totalScenarioWorkers(scenarios []benchmarkScenarioSpec) int {
	total := 0
	for _, scenario := range scenarios {
		total += scenario.Workers
	}
	return total
}

type benchmarkRecorder struct {
	mu           sync.Mutex
	phaseStarted time.Time
	bucketSize   time.Duration
	bucketCount  int
	latenciesMs  []float64
	successes    int
	errors       int
	sumMs        float64
	minMs        float64
	maxMs        float64
	errorCounts  map[string]int
	buckets      map[int]*benchmarkBucket
}

type benchmarkBucket struct {
	latenciesMs []float64
	errors      int
}

func newBenchmarkRecorder(phaseStarted time.Time, bucketSize, duration time.Duration) *benchmarkRecorder {
	bucketCount := int(duration / bucketSize)
	if duration%bucketSize != 0 {
		bucketCount++
	}
	if bucketCount <= 0 {
		bucketCount = 1
	}

	return &benchmarkRecorder{
		phaseStarted: phaseStarted,
		bucketSize:   bucketSize,
		bucketCount:  bucketCount,
		minMs:        -1,
		errorCounts:  make(map[string]int),
		buckets:      make(map[int]*benchmarkBucket, bucketCount),
	}
}

func (r *benchmarkRecorder) bucketAt(at time.Time) *benchmarkBucket {
	idx := int(at.Sub(r.phaseStarted) / r.bucketSize)
	if idx < 0 {
		idx = 0
	}
	if idx >= r.bucketCount {
		idx = r.bucketCount - 1
	}
	bucket := r.buckets[idx]
	if bucket == nil {
		bucket = &benchmarkBucket{}
		r.buckets[idx] = bucket
	}
	return bucket
}

func (r *benchmarkRecorder) Record(at time.Time, latency time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket := r.bucketAt(at)
	if err != nil {
		r.errors++
		r.errorCounts[truncateBenchmarkError(err.Error())]++
		bucket.errors++
		return
	}

	ms := durationMillis(latency)
	r.successes++
	r.sumMs += ms
	r.latenciesMs = append(r.latenciesMs, ms)
	bucket.latenciesMs = append(bucket.latenciesMs, ms)
	if r.minMs < 0 || ms < r.minMs {
		r.minMs = ms
	}
	if ms > r.maxMs {
		r.maxMs = ms
	}
}

func (r *benchmarkRecorder) Report(spec benchmarkScenarioSpec, duration time.Duration) benchmarkScenarioReport {
	r.mu.Lock()
	latencies := append([]float64(nil), r.latenciesMs...)
	successes := r.successes
	errors := r.errors
	sumMs := r.sumMs
	minMs := r.minMs
	maxMs := r.maxMs
	errorCounts := make(map[string]int, len(r.errorCounts))
	for msg, count := range r.errorCounts {
		errorCounts[msg] = count
	}
	buckets := make(map[int]benchmarkBucket, len(r.buckets))
	for idx, bucket := range r.buckets {
		buckets[idx] = benchmarkBucket{
			latenciesMs: append([]float64(nil), bucket.latenciesMs...),
			errors:      bucket.errors,
		}
	}
	phaseStarted := r.phaseStarted
	bucketSize := r.bucketSize
	bucketCount := r.bucketCount
	r.mu.Unlock()

	sort.Float64s(latencies)
	attempts := successes + errors
	report := benchmarkScenarioReport{
		Name:                  spec.Name,
		Signature:             spec.Signature,
		Workers:               spec.Workers,
		InvocationCount:       len(spec.Invocations),
		Attempts:              attempts,
		Samples:               successes,
		Errors:                errors,
		RequestsPerSecond:     roundFloat(float64(attempts) / duration.Seconds()),
		SuccessesPerSecond:    roundFloat(float64(successes) / duration.Seconds()),
		SuccessRatePct:        100,
		BucketDurationSeconds: roundSeconds(bucketSize),
		TopErrors:             topBenchmarkErrors(errorCounts, 4),
	}
	if attempts > 0 {
		report.SuccessRatePct = roundFloat((float64(successes) / float64(attempts)) * 100)
	}

	for idx := 0; idx < bucketCount; idx++ {
		bucket := buckets[idx]
		point := benchmarkTimeseriesPoint{
			Timestamp: phaseStarted.Add(time.Duration(idx) * bucketSize).Format(time.RFC3339),
			Samples:   len(bucket.latenciesMs),
			Errors:    bucket.errors,
		}
		if len(bucket.latenciesMs) > 0 {
			sort.Float64s(bucket.latenciesMs)
			p50 := roundFloat(percentileValue(bucket.latenciesMs, 0.50))
			p9995 := roundFloat(percentileValue(bucket.latenciesMs, 0.9995))
			p9999 := roundFloat(percentileValue(bucket.latenciesMs, 0.9999))
			point.P50Ms = &p50
			point.P9995Ms = &p9995
			point.P9999Ms = &p9999
		}
		report.Timeseries = append(report.Timeseries, point)
	}

	if successes == 0 {
		report.Notes = append(report.Notes, "No successful samples were recorded.")
		return report
	}

	report.MinMs = roundFloat(minMs)
	report.MeanMs = roundFloat(sumMs / float64(successes))
	report.MaxMs = roundFloat(maxMs)
	report.P50Ms = roundFloat(percentileValue(latencies, 0.50))
	report.P9995Ms = roundFloat(percentileValue(latencies, 0.9995))
	report.P9999Ms = roundFloat(percentileValue(latencies, 0.9999))
	for _, q := range []struct {
		Label string
		Value float64
	}{
		{Label: "p50", Value: 0.50},
		{Label: "p90", Value: 0.90},
		{Label: "p95", Value: 0.95},
		{Label: "p99", Value: 0.99},
		{Label: "p99.9", Value: 0.999},
		{Label: "p99.95", Value: 0.9995},
		{Label: "p99.99", Value: 0.9999},
	} {
		report.Percentiles = append(report.Percentiles, benchmarkPercentilePoint{
			Label: q.Label,
			Ms:    roundFloat(percentileValue(latencies, q.Value)),
		})
	}
	if successes < 10_000 {
		report.Notes = append(report.Notes, "p99.99 is directionally useful but under 10k samples.")
	}
	if successes < 2_000 {
		report.Notes = append(report.Notes, "p99.95 is based on fewer than 2k samples.")
	}
	return report
}

func runBenchmarkPhase(
	t *testing.T,
	client *http.Client,
	rpcURL string,
	scenarios []benchmarkScenarioSpec,
	duration time.Duration,
	bucketDuration time.Duration,
	record bool,
) []benchmarkScenarioReport {
	t.Helper()

	phaseStarted := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	recorders := make(map[string]*benchmarkRecorder, len(scenarios))
	cursors := make(map[string]*atomic.Uint64, len(scenarios))
	for _, scenario := range scenarios {
		recorders[scenario.Name] = newBenchmarkRecorder(phaseStarted, bucketDuration, duration)
		cursors[scenario.Name] = &atomic.Uint64{}
	}

	var wg sync.WaitGroup
	for _, scenario := range scenarios {
		if len(scenario.Invocations) == 0 {
			t.Fatalf("benchmark scenario %s has no invocations", scenario.Name)
		}
		for worker := 0; worker < scenario.Workers; worker++ {
			wg.Add(1)
			go func(spec benchmarkScenarioSpec) {
				defer wg.Done()
				recorder := recorders[spec.Name]
				cursor := cursors[spec.Name]
				for {
					select {
					case <-ctx.Done():
						return
					default:
					}

					idx := int(cursor.Add(1)-1) % len(spec.Invocations)
					invocation := spec.Invocations[idx]
					reqCtx, cancelReq := context.WithTimeout(ctx, spec.Timeout)
					start := time.Now()
					err := invocation.Execute(reqCtx, client, rpcURL)
					latency := time.Since(start)
					finishedAt := time.Now().UTC()
					cancelReq()
					if record {
						if err != nil && ctx.Err() != nil {
							return
						}
						recorder.Record(finishedAt, latency, err)
					}
				}
			}(scenario)
		}
	}

	<-ctx.Done()
	wg.Wait()

	if !record {
		return nil
	}

	reports := make([]benchmarkScenarioReport, 0, len(scenarios))
	for _, scenario := range scenarios {
		reports = append(reports, recorders[scenario.Name].Report(scenario, duration))
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].Signature < reports[j].Signature
	})
	return reports
}

func newBenchmarkHTTPClient(workerCount int, timeout time.Duration) (*http.Client, func()) {
	maxConns := maxInt(32, workerCount*4)
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	return client, transport.CloseIdleConnections
}

func rpcCallResponseWithClient(ctx context.Context, client *http.Client, rpcURL, method string, params interface{}) (rpcResponse, error) {
	reqBody, err := json.Marshal(rpcEnvelope{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return rpcResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return rpcResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return rpcResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bz, _ := io.ReadAll(resp.Body)
		return rpcResponse{}, fmt.Errorf("rpc http status %d: %s", resp.StatusCode, string(bz))
	}

	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return rpcResponse{}, err
	}
	return out, nil
}

func rpcBatchCallWithClient(ctx context.Context, client *http.Client, rpcURL string, requests []rpcEnvelope) ([]rpcResponse, error) {
	reqBody, err := json.Marshal(requests)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bz, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("rpc http status %d: %s", resp.StatusCode, string(bz))
	}

	var out []rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

type benchmarkReport struct {
	Suite                  string                     `json:"suite"`
	GeneratedAt            string                     `json:"generated_at"`
	OutputDir              string                     `json:"output_dir"`
	BucketDurationSeconds  float64                    `json:"bucket_duration_seconds"`
	WarmupDurationSeconds  float64                    `json:"warmup_duration_seconds"`
	MeasureDurationSeconds float64                    `json:"measure_duration_seconds"`
	Environment            benchmarkEnvironmentReport `json:"environment"`
	Seed                   benchmarkSeedReport        `json:"seed"`
	Fixtures               benchmarkFixtureReport     `json:"fixtures"`
	Gateway                benchmarkGatewayReport     `json:"gateway"`
	Totals                 benchmarkTotalsReport      `json:"totals"`
	Scenarios              []benchmarkScenarioReport  `json:"scenarios"`
}

type benchmarkEnvironmentReport struct {
	SourceRPC        string             `json:"source_rpc"`
	CometRPC         string             `json:"comet_rpc"`
	GRPCAddr         string             `json:"grpc_addr"`
	ChainID          string             `json:"chain_id"`
	EthChainID       int64              `json:"eth_chain_id"`
	OfflineAfterSync bool               `json:"offline_after_sync"`
	NoFile           benchmarkNoFileCap `json:"nofile"`
}

type benchmarkNoFileCap struct {
	Soft    uint64 `json:"soft"`
	Hard    uint64 `json:"hard"`
	Target  uint64 `json:"target"`
	Raised  bool   `json:"raised"`
	Warning string `json:"warning,omitempty"`
}

type benchmarkSeedReport struct {
	DurationSecondsPerWorkload float64                 `json:"duration_seconds_per_workload"`
	SettleDelaySeconds         float64                 `json:"settle_delay_seconds"`
	AccountsNum                int                     `json:"accounts_num"`
	InternalCallIterations     int                     `json:"internal_call_iterations"`
	HeadBefore                 int64                   `json:"head_before"`
	HeadAfter                  int64                   `json:"head_after"`
	GeneratedFrom              int64                   `json:"generated_from"`
	GeneratedTo                int64                   `json:"generated_to"`
	Workloads                  []benchmarkSeedWorkload `json:"workloads"`
}

type benchmarkSeedWorkload struct {
	Name            string   `json:"name"`
	Command         string   `json:"command"`
	Args            []string `json:"args,omitempty"`
	DurationSeconds float64  `json:"duration_seconds"`
}

type benchmarkFixtureReport struct {
	HeaderBlocks int `json:"header_blocks"`
	FullBlocks   int `json:"full_blocks"`
	RangeFilters int `json:"range_filters"`
	Transactions int `json:"transactions"`
	Batches      int `json:"batches"`
	TraceHashes  int `json:"trace_hashes"`
}

type benchmarkGatewayReport struct {
	RPCURL       string             `json:"rpc_url"`
	DataDir      string             `json:"data_dir"`
	LogPath      string             `json:"log_path"`
	StatusBefore syncStatusResponse `json:"status_before"`
	StatusAfter  syncStatusResponse `json:"status_after"`
}

type benchmarkTotalsReport struct {
	Attempts int     `json:"attempts"`
	Samples  int     `json:"samples"`
	Errors   int     `json:"errors"`
	RPS      float64 `json:"rps"`
}

type benchmarkScenarioReport struct {
	Name                  string                     `json:"name"`
	Signature             string                     `json:"signature"`
	Workers               int                        `json:"workers"`
	InvocationCount       int                        `json:"invocation_count"`
	Attempts              int                        `json:"attempts"`
	Samples               int                        `json:"samples"`
	Errors                int                        `json:"errors"`
	SuccessRatePct        float64                    `json:"success_rate_pct"`
	RequestsPerSecond     float64                    `json:"requests_per_second"`
	SuccessesPerSecond    float64                    `json:"successes_per_second"`
	BucketDurationSeconds float64                    `json:"bucket_duration_seconds"`
	MinMs                 float64                    `json:"min_ms,omitempty"`
	MeanMs                float64                    `json:"mean_ms,omitempty"`
	MaxMs                 float64                    `json:"max_ms,omitempty"`
	P50Ms                 float64                    `json:"p50_ms,omitempty"`
	P9995Ms               float64                    `json:"p99_95_ms,omitempty"`
	P9999Ms               float64                    `json:"p99_99_ms,omitempty"`
	Percentiles           []benchmarkPercentilePoint `json:"percentiles,omitempty"`
	Timeseries            []benchmarkTimeseriesPoint `json:"timeseries,omitempty"`
	TopErrors             []benchmarkErrorCount      `json:"top_errors,omitempty"`
	Notes                 []string                   `json:"notes,omitempty"`
}

type benchmarkPercentilePoint struct {
	Label string  `json:"label"`
	Ms    float64 `json:"ms"`
}

type benchmarkTimeseriesPoint struct {
	Timestamp string   `json:"timestamp"`
	Samples   int      `json:"samples"`
	Errors    int      `json:"errors"`
	P50Ms     *float64 `json:"p50_ms,omitempty"`
	P9995Ms   *float64 `json:"p99_95_ms,omitempty"`
	P9999Ms   *float64 `json:"p99_99_ms,omitempty"`
}

type benchmarkErrorCount struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

func summarizeScenarioReports(reports []benchmarkScenarioReport) benchmarkTotalsReport {
	var total benchmarkTotalsReport
	for _, report := range reports {
		total.Attempts += report.Attempts
		total.Samples += report.Samples
		total.Errors += report.Errors
		total.RPS += report.RequestsPerSecond
	}
	total.RPS = roundFloat(total.RPS)
	return total
}

func topBenchmarkErrors(counts map[string]int, limit int) []benchmarkErrorCount {
	if len(counts) == 0 {
		return nil
	}

	out := make([]benchmarkErrorCount, 0, len(counts))
	for message, count := range counts {
		out = append(out, benchmarkErrorCount{Message: message, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Message < out[j].Message
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func benchmarkScenarioFailures(reports []benchmarkScenarioReport) []string {
	var failures []string
	for _, report := range reports {
		if report.Errors == 0 {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s=%d", report.Name, report.Errors))
	}
	return failures
}

type benchmarkHTMLTemplateData struct {
	CSVPath template.JSStr
}

func writeBenchmarkArtifacts(t *testing.T, artifactsDir string, report benchmarkReport) {
	t.Helper()

	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("create artifacts dir: %v", err)
	}

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal benchmark report: %v", err)
	}
	jsonPath := filepath.Join(artifactsDir, "report.json")
	if err := os.WriteFile(jsonPath, reportJSON, 0o644); err != nil {
		t.Fatalf("write benchmark json report: %v", err)
	}

	csvPath := filepath.Join(artifactsDir, "timeseries.csv")
	if err := writeBenchmarkTimeseriesCSV(csvPath, report); err != nil {
		t.Fatalf("write benchmark csv report: %v", err)
	}

	tpl, err := template.New("benchmark_report").Parse(benchmarkReportHTML)
	if err != nil {
		t.Fatalf("parse benchmark html template: %v", err)
	}

	var htmlOut bytes.Buffer
	if err := tpl.Execute(&htmlOut, benchmarkHTMLTemplateData{
		CSVPath: template.JSStr("timeseries.csv"),
	}); err != nil {
		t.Fatalf("render benchmark html report: %v", err)
	}

	htmlPath := filepath.Join(artifactsDir, "report.html")
	if err := os.WriteFile(htmlPath, htmlOut.Bytes(), 0o644); err != nil {
		t.Fatalf("write benchmark html report: %v", err)
	}
}

func writeBenchmarkTimeseriesCSV(path string, report benchmarkReport) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	if err := writer.Write([]string{
		"scenario_key",
		"signature",
		"timestamp",
		"p50_ms",
		"p99_95_ms",
		"p99_99_ms",
		"samples",
		"errors",
	}); err != nil {
		return err
	}

	for _, scenario := range report.Scenarios {
		for _, point := range scenario.Timeseries {
			record := []string{
				scenario.Name,
				scenario.Signature,
				point.Timestamp,
				formatCSVFloat(point.P50Ms),
				formatCSVFloat(point.P9995Ms),
				formatCSVFloat(point.P9999Ms),
				strconv.Itoa(point.Samples),
				strconv.Itoa(point.Errors),
			}
			if err := writer.Write(record); err != nil {
				return err
			}
		}
	}

	writer.Flush()
	return writer.Error()
}

func prepareBenchmarkOutputDir(t *testing.T) string {
	t.Helper()

	if dir := strings.TrimSpace(os.Getenv("WEB3INJ_BENCH_OUTPUT_DIR")); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create benchmark output dir %s: %v", dir, err)
		}
		return dir
	}

	dir := filepath.Join(
		projectRoot(t),
		"docs",
		"benchmarks",
		time.Now().UTC().Format("20060102T150405Z"),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create benchmark output dir %s: %v", dir, err)
	}
	return dir
}

func ensureOpenFileLimit(target uint64) benchmarkNoFileCap {
	limit := benchmarkNoFileCap{Target: target}

	var current syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &current); err != nil {
		limit.Warning = err.Error()
		return limit
	}

	limit.Soft = current.Cur
	limit.Hard = current.Max
	if current.Cur >= target {
		return limit
	}

	desired := target
	if desired > current.Max {
		desired = current.Max
		limit.Warning = fmt.Sprintf("hard nofile cap %d below requested target %d", current.Max, target)
	}
	if desired <= current.Cur {
		return limit
	}

	current.Cur = desired
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &current); err != nil {
		if limit.Warning != "" {
			limit.Warning += "; "
		}
		limit.Warning += err.Error()
		return limit
	}

	limit.Raised = true
	var updated syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &updated); err == nil {
		limit.Soft = updated.Cur
		limit.Hard = updated.Max
	} else {
		limit.Soft = desired
	}
	return limit
}

func percentileValue(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int((q*float64(len(sorted)))+0.999999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func roundSeconds(d time.Duration) float64 {
	return roundFloat(d.Seconds())
}

func roundFloat(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func formatCSVFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 3, 64)
}

func truncateBenchmarkError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= 180 {
		return msg
	}
	return msg[:177] + "..."
}

func getenvUint64(key string, fallback uint64) uint64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		return fallback
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
