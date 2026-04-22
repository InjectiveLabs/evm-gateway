package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultWSBenchGatewayRPCPort  = 8946
	defaultWSBenchGatewayWSPort   = 8947
	defaultWSBenchClients         = 100
	defaultWSBenchFetchJobs       = 8
	defaultWSBenchSeedAccountsNum = 64
	defaultWSBenchSeedTxs         = 100
	defaultWSBenchWarmupDuration  = 10 * time.Second
	defaultWSBenchMeasureDuration = 30 * time.Second
	defaultWSBenchMinNoFile       = 8192
)

func TestWSStreamBenchmarkSuite(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E_WS_BENCH") != "1" {
		t.Skip("set WEB3INJ_E2E_WS_BENCH=1 to run websocket stream benchmark e2e")
	}

	ctx := context.Background()
	cfg := loadWSBenchmarkConfig(t)
	limit := ensureOpenFileLimit(cfg.MinNoFile)
	if strings.TrimSpace(limit.Warning) != "" {
		t.Logf("nofile warning: %s", limit.Warning)
	}

	artifactsDir := prepareWSBenchmarkOutputDir(t)
	t.Logf("ws benchmark artifacts dir: %s", artifactsDir)

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
		t.Fatalf("query source head before ws benchmark: %v", err)
	}

	gatewayBin := buildGatewayBinary(t)
	stresserBin := buildChainStresserBinary(t, stresserRoot)
	gatewayDataDir := filepath.Join(artifactsDir, "gateway")
	syncStarted := time.Now()
	proc := startGateway(t, gatewayStartConfig{
		BinaryPath:             gatewayBin,
		DataDir:                gatewayDataDir,
		RPCPort:                cfg.GatewayRPCPort,
		WSPort:                 cfg.GatewayWSPort,
		Earliest:               maxInt64(1, headBefore),
		FetchJobs:              cfg.FetchJobs,
		CometRPC:               cfg.CometRPC,
		GRPCAddr:               cfg.GRPCAddr,
		ChainID:                cfg.ChainID,
		EVMChainID:             strconv.FormatInt(cfg.EthChainID, 10),
		EnableSync:             true,
		EnableRPC:              true,
		VirtualizeCosmosEvents: true,
		APIList:                "eth,net,web3,debug,inj",
		WaitTimeout:            90 * time.Second,
	})
	defer proc.Stop(t)

	waitForCondition(t, 4*time.Minute, func() (bool, error) {
		st, err := proc.Status(ctx)
		if err != nil {
			return false, err
		}
		tipSyncPhase := st.Phase == "forward_sync" || st.Phase == "parallel_gap_tip_sync"
		return tipSyncPhase && st.GapsRemaining == 0 && st.LastSyncedBlock >= headBefore, nil
	}, "ws benchmark gateway did not reach forward tip sync")
	syncDuration := time.Since(syncStarted)

	statusBefore, err := proc.Status(ctx)
	if err != nil {
		t.Fatalf("query pre-benchmark status: %v", err)
	}

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/", cfg.GatewayWSPort)
	subscriptionStatuses := []wsSubscriptionStatus{
		{Name: "newHeads", Status: "measured"},
		{Name: "logs", Status: "measured"},
		{Name: "newPendingTransactions", Status: "measured"},
		probeWSSubscription(ctx, t, wsURL, wsSubscriptionSpec{Name: "syncing", Params: []interface{}{"syncing"}}),
	}

	specs := []wsSubscriptionSpec{
		{Name: "newHeads", Params: []interface{}{"newHeads"}, Required: true},
		{Name: "logs", Params: []interface{}{"logs", map[string]interface{}{}}, Required: true},
		{Name: "newPendingTransactions", Params: []interface{}{"newPendingTransactions"}, Required: true},
	}
	recorder := newWSBenchRecorder(specs)

	measureStart := time.Now().Add(cfg.WarmupDuration)
	measureEnd := measureStart.Add(cfg.MeasureDuration)
	clientsCtx, cancelClients := context.WithDeadline(context.Background(), measureEnd.Add(5*time.Second))
	defer cancelClients()

	var clientsWG sync.WaitGroup
	readyC := make(chan wsBenchClientReady, cfg.Clients)
	for clientID := 0; clientID < cfg.Clients; clientID++ {
		clientsWG.Add(1)
		go func(id int) {
			defer clientsWG.Done()
			runWSBenchmarkClient(clientsCtx, wsURL, id, specs, recorder, readyC, measureStart, measureEnd)
		}(clientID)
	}

	for clientID := 0; clientID < cfg.Clients; clientID++ {
		select {
		case ready := <-readyC:
			if ready.Err != nil {
				cancelClients()
				clientsWG.Wait()
				t.Fatalf("ws benchmark client %d failed to subscribe: %v", ready.ClientID, ready.Err)
			}
		case <-time.After(30 * time.Second):
			cancelClients()
			clientsWG.Wait()
			t.Fatal("timed out waiting for websocket clients to subscribe")
		}
	}
	t.Logf("all %d websocket clients subscribed; warmup=%s measure=%s", cfg.Clients, cfg.WarmupDuration, cfg.MeasureDuration)

	mixedConfigPath := filepath.Join(artifactsDir, "mixed-ws-stream.yaml")
	writeWSBenchmarkMixedPayloadConfig(t, mixedConfigPath)
	stressDuration := cfg.WarmupDuration + cfg.MeasureDuration + 5*time.Second
	stressCtx, cancelStress := context.WithTimeout(context.Background(), stressDuration)
	stressRun := startWSBenchmarkStresser(
		stressCtx,
		stresserRoot,
		stresserBin,
		append([]string{
			"tx-mixed-payload", mixedConfigPath,
			"--chain-id", cfg.ChainID,
			"--eth-chain-id", strconv.FormatInt(cfg.EthChainID, 10),
			"--node-addr", endpointHostPort(t, cfg.CometRPC),
			"--grpc-addr", cfg.GRPCAddr,
			"--accounts", accountsPath,
			"--accounts-num", strconv.Itoa(cfg.SeedAccountsNum),
			"--transactions", strconv.Itoa(cfg.SeedTransactions),
			"--min-gas-price", getenv("WEB3INJ_E2E_MIN_GAS_PRICE", "160000000inj"),
		}, wsBenchmarkRateArgs(cfg.RateTPS)...),
	)

	<-clientsCtx.Done()
	cancelStress()
	clientsWG.Wait()
	stressReport := <-stressRun.Done

	statusAfter, err := proc.Status(ctx)
	if err != nil {
		t.Fatalf("query post-benchmark status: %v", err)
	}
	headAfter, err := ethBlockNumber(ctx, cfg.SourceRPC)
	if err != nil {
		t.Fatalf("query source head after ws benchmark: %v", err)
	}

	subscriptionReports := recorder.Reports(cfg.MeasureDuration, cfg.Clients)
	report := wsBenchmarkReport{
		Suite:                  "ws-stream-benchmark",
		GeneratedAt:            time.Now().UTC().Format(time.RFC3339),
		OutputDir:              artifactsDir,
		WarmupDurationSeconds:  roundSeconds(cfg.WarmupDuration),
		MeasureDurationSeconds: roundSeconds(cfg.MeasureDuration),
		Environment: wsBenchmarkEnvironmentReport{
			SourceRPC:              cfg.SourceRPC,
			CometRPC:               cfg.CometRPC,
			GRPCAddr:               cfg.GRPCAddr,
			ChainID:                cfg.ChainID,
			EthChainID:             cfg.EthChainID,
			VirtualizeCosmosEvents: true,
			NoFile:                 limit,
		},
		Seed: wsBenchmarkSeedReport{
			StressDurationSeconds:  roundSeconds(stressDuration),
			AccountsNum:            cfg.SeedAccountsNum,
			TransactionsPerAccount: cfg.SeedTransactions,
			RateTPS:                cfg.RateTPS,
			HeadBefore:             headBefore,
			HeadAfter:              headAfter,
			Workloads:              []wsBenchmarkStressWorkload{stressReport},
		},
		Gateway: wsBenchmarkGatewayReport{
			RPCURL:              proc.RPCURL(),
			WSURL:               wsURL,
			DataDir:             gatewayDataDir,
			LogPath:             proc.logPath,
			SyncDurationSeconds: roundSeconds(syncDuration),
			StatusBefore:        statusBefore,
			StatusAfter:         statusAfter,
		},
		SubscriptionStatuses: subscriptionStatuses,
		Subscriptions:        subscriptionReports,
		GlobalErrors:         recorder.GlobalErrors(8),
		Totals:               summarizeWSSubscriptionReports(subscriptionReports),
	}
	writeWSBenchmarkReport(t, artifactsDir, report)

	var failures []string
	if strings.TrimSpace(stressReport.Error) != "" {
		failures = append(failures, "stresser="+stressReport.Error)
	}
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		report := findWSSubscriptionReport(subscriptionReports, spec.Name)
		if report == nil || report.Notifications == 0 {
			failures = append(failures, spec.Name+"=no notifications")
		}
		if report != nil && report.Errors > 0 {
			failures = append(failures, fmt.Sprintf("%s=%d errors", spec.Name, report.Errors))
		}
	}
	if len(report.GlobalErrors) > 0 {
		failures = append(failures, "global websocket errors recorded")
	}
	if len(failures) > 0 {
		t.Fatalf("ws stream benchmark failed: %s (see %s/report.json)", strings.Join(failures, ", "), artifactsDir)
	}
	t.Logf("ws stream benchmark report written to %s", artifactsDir)
}

type wsBenchmarkConfig struct {
	SourceRPC        string
	CometRPC         string
	GRPCAddr         string
	ChainID          string
	EthChainID       int64
	GatewayRPCPort   int
	GatewayWSPort    int
	FetchJobs        int
	Clients          int
	WarmupDuration   time.Duration
	MeasureDuration  time.Duration
	SeedAccountsNum  int
	SeedTransactions int
	RateTPS          float64
	MinNoFile        uint64
}

func loadWSBenchmarkConfig(t *testing.T) wsBenchmarkConfig {
	t.Helper()

	if v := strings.TrimSpace(os.Getenv("WEB3INJ_VIRTUALIZE_COSMOS_EVENTS")); v != "" && !envBool("WEB3INJ_VIRTUALIZE_COSMOS_EVENTS") {
		t.Fatal("WEB3INJ_VIRTUALIZE_COSMOS_EVENTS must be true for the websocket stream benchmark")
	}

	sourceRPC := getenv("WEB3INJ_E2E_SOURCE_RPC", defaultSourceRPC)
	ethChainID, err := ethChainID(context.Background(), sourceRPC)
	if err != nil {
		t.Fatalf("query eth chain id: %v", err)
	}

	return wsBenchmarkConfig{
		SourceRPC:        sourceRPC,
		CometRPC:         getenv("WEB3INJ_COMET_RPC", defaultCometRPC),
		GRPCAddr:         resolveParityGRPCAddr(t),
		EthChainID:       ethChainID,
		GatewayRPCPort:   getenvInt("WEB3INJ_WS_BENCH_GATEWAY_RPC_PORT", defaultWSBenchGatewayRPCPort),
		GatewayWSPort:    getenvInt("WEB3INJ_WS_BENCH_GATEWAY_WS_PORT", defaultWSBenchGatewayWSPort),
		FetchJobs:        getenvInt("WEB3INJ_WS_BENCH_FETCH_JOBS", defaultWSBenchFetchJobs),
		Clients:          getenvInt("WEB3INJ_WS_BENCH_CLIENTS", defaultWSBenchClients),
		WarmupDuration:   getenvDurationSeconds("WEB3INJ_WS_BENCH_WARMUP_SEC", defaultWSBenchWarmupDuration),
		MeasureDuration:  getenvDurationSeconds("WEB3INJ_WS_BENCH_DURATION_SEC", defaultWSBenchMeasureDuration),
		SeedAccountsNum:  getenvInt("WEB3INJ_WS_BENCH_SEED_ACCOUNTS_NUM", defaultWSBenchSeedAccountsNum),
		SeedTransactions: getenvInt("WEB3INJ_WS_BENCH_TRANSACTIONS", defaultWSBenchSeedTxs),
		RateTPS:          getenvFloat64("WEB3INJ_WS_BENCH_RATE_TPS", 0),
		MinNoFile:        getenvUint64("WEB3INJ_WS_BENCH_MIN_NOFILE", defaultWSBenchMinNoFile),
	}
}

func prepareWSBenchmarkOutputDir(t *testing.T) string {
	t.Helper()

	if dir := strings.TrimSpace(os.Getenv("WEB3INJ_WS_BENCH_OUTPUT_DIR")); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create ws benchmark output dir %s: %v", dir, err)
		}
		return dir
	}

	dir := filepath.Join(
		projectRoot(t),
		"docs",
		"benchmarks",
		"ws-stream-"+time.Now().UTC().Format("20060102T150405Z"),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create ws benchmark output dir %s: %v", dir, err)
	}
	return dir
}

func writeWSBenchmarkMixedPayloadConfig(t *testing.T, path string) {
	t.Helper()

	body := []byte(strings.TrimSpace(`
bank_send:
  frequency: 0.35
  send_amount: "1inj"
bank_multi_send:
  frequency: 0.25
  send_amount: "1inj"
  num_targets: 3
eth_send:
  frequency: 0.25
  send_amount: "1inj"
eth_call:
  frequency: 0.15
`) + "\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write ws benchmark mixed payload config: %v", err)
	}
}

func wsBenchmarkRateArgs(rate float64) []string {
	if rate <= 0 {
		return nil
	}
	return []string{"--rate-tps", strconv.FormatFloat(rate, 'f', -1, 64)}
}

type wsBenchmarkStressRun struct {
	Done chan wsBenchmarkStressWorkload
}

type wsBenchmarkStressWorkload struct {
	Name            string   `json:"name"`
	Command         string   `json:"command"`
	Args            []string `json:"args,omitempty"`
	DurationSeconds float64  `json:"duration_seconds"`
	Error           string   `json:"error,omitempty"`
}

func startWSBenchmarkStresser(ctx context.Context, stresserRoot, binPath string, args []string) wsBenchmarkStressRun {
	done := make(chan wsBenchmarkStressWorkload, 1)
	start := time.Now()
	go func() {
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Dir = stresserRoot
		output, err := cmd.CombinedOutput()

		report := wsBenchmarkStressWorkload{
			Name:            "mixed_native_evm_tip_traffic",
			Command:         "tx-mixed-payload",
			Args:            args,
			DurationSeconds: roundSeconds(time.Since(start)),
		}
		if ctx.Err() == context.DeadlineExceeded || ctx.Err() == context.Canceled {
			done <- report
			return
		}
		if err != nil {
			report.Error = truncateBenchmarkError(fmt.Sprintf("%v: %s", err, string(output)))
		}
		done <- report
	}()
	return wsBenchmarkStressRun{Done: done}
}

type wsSubscriptionSpec struct {
	Name     string
	Params   []interface{}
	Required bool
}

type wsBenchClientReady struct {
	ClientID int
	Err      error
}

type wsBenchMessage struct {
	JSONRPC string                    `json:"jsonrpc"`
	ID      int                       `json:"id,omitempty"`
	Result  json.RawMessage           `json:"result,omitempty"`
	Error   *rpcError                 `json:"error,omitempty"`
	Method  string                    `json:"method,omitempty"`
	Params  *wsBenchNotificationParam `json:"params,omitempty"`
}

type wsBenchNotificationParam struct {
	Subscription string               `json:"subscription"`
	Result       json.RawMessage      `json:"result"`
	Metadata     *wsBenchNotification `json:"metadata"`
}

type wsBenchNotification struct {
	EmittedAtUnixNano int64 `json:"emittedAtUnixNano"`
}

func runWSBenchmarkClient(
	ctx context.Context,
	wsURL string,
	clientID int,
	specs []wsSubscriptionSpec,
	recorder *wsBenchRecorder,
	readyC chan<- wsBenchClientReady,
	measureStart time.Time,
	measureEnd time.Time,
) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		readyC <- wsBenchClientReady{ClientID: clientID, Err: err}
		return
	}
	defer closeWSBenchmarkConn(conn)

	requestNames := make(map[int]string, len(specs))
	for i, spec := range specs {
		id := i + 1
		requestNames[id] = spec.Name
		if err := conn.WriteJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "eth_subscribe",
			"params":  spec.Params,
		}); err != nil {
			readyC <- wsBenchClientReady{ClientID: clientID, Err: err}
			return
		}
	}

	subscriptionNames := make(map[string]string, len(specs))
	ackCtx, cancelAck := context.WithTimeout(ctx, 15*time.Second)
	defer cancelAck()
	for len(subscriptionNames) < len(specs) {
		msg, err := readWSBenchmarkMessage(ackCtx, conn)
		if err != nil {
			readyC <- wsBenchClientReady{ClientID: clientID, Err: err}
			return
		}
		if msg.ID == 0 {
			continue
		}
		name := requestNames[msg.ID]
		if name == "" {
			continue
		}
		if msg.Error != nil {
			readyC <- wsBenchClientReady{
				ClientID: clientID,
				Err:      fmt.Errorf("%s subscribe failed: %d %s", name, msg.Error.Code, msg.Error.Message),
			}
			return
		}
		var subID string
		if err := json.Unmarshal(msg.Result, &subID); err != nil {
			readyC <- wsBenchClientReady{ClientID: clientID, Err: fmt.Errorf("%s subscribe result: %w", name, err)}
			return
		}
		subscriptionNames[subID] = name
	}
	readyC <- wsBenchClientReady{ClientID: clientID}

	readCtx, cancelRead := context.WithDeadline(ctx, measureEnd.Add(time.Second))
	defer cancelRead()
	for {
		if time.Now().After(measureEnd) {
			return
		}
		msg, err := readWSBenchmarkMessage(readCtx, conn)
		if err != nil {
			if readCtx.Err() != nil || time.Now().After(measureEnd) {
				return
			}
			recorder.RecordGlobalError(err.Error())
			return
		}
		now := time.Now()
		if now.Before(measureStart) {
			continue
		}
		if now.After(measureEnd) {
			return
		}
		if msg.Method != "eth_subscription" || msg.Params == nil {
			continue
		}
		name := subscriptionNames[msg.Params.Subscription]
		if name == "" {
			recorder.RecordGlobalError("unknown subscription id " + msg.Params.Subscription)
			continue
		}
		if msg.Params.Metadata == nil || msg.Params.Metadata.EmittedAtUnixNano <= 0 {
			recorder.RecordError(name, "missing emittedAtUnixNano metadata")
			continue
		}
		latency := time.Duration(now.UnixNano() - msg.Params.Metadata.EmittedAtUnixNano)
		if latency < 0 {
			latency = 0
		}
		recorder.Record(name, latency)
	}
}

func readWSBenchmarkMessage(ctx context.Context, conn *websocket.Conn) (wsBenchMessage, error) {
	if ctx.Err() != nil {
		return wsBenchMessage{}, ctx.Err()
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return wsBenchMessage{}, ctx.Err()
		}
		if isNetTimeout(err) {
			return wsBenchMessage{}, context.DeadlineExceeded
		}
		return wsBenchMessage{}, err
	}

	var msg wsBenchMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return wsBenchMessage{}, err
	}
	return msg, nil
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return err != nil && (os.IsTimeout(err) || errors.As(err, &netErr) && netErr.Timeout())
}

func probeWSSubscription(ctx context.Context, t *testing.T, wsURL string, spec wsSubscriptionSpec) wsSubscriptionStatus {
	t.Helper()

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(probeCtx, wsURL, nil)
	if err != nil {
		return wsSubscriptionStatus{Name: spec.Name, Status: "connect_error", Error: err.Error()}
	}
	defer closeWSBenchmarkConn(conn)

	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params":  spec.Params,
	}); err != nil {
		return wsSubscriptionStatus{Name: spec.Name, Status: "write_error", Error: err.Error()}
	}

	msg, err := readWSBenchmarkMessage(probeCtx, conn)
	if err != nil {
		return wsSubscriptionStatus{Name: spec.Name, Status: "read_error", Error: err.Error()}
	}
	if msg.Error != nil {
		return wsSubscriptionStatus{
			Name:   spec.Name,
			Status: "unsupported",
			Error:  fmt.Sprintf("%d %s", msg.Error.Code, msg.Error.Message),
		}
	}
	return wsSubscriptionStatus{Name: spec.Name, Status: "supported"}
}

func closeWSBenchmarkConn(conn *websocket.Conn) {
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	_ = conn.Close()
}

type wsBenchRecorder struct {
	mu           sync.Mutex
	stats        map[string]*wsBenchSubscriptionStats
	globalErrors map[string]int
}

type wsBenchSubscriptionStats struct {
	notifications int
	errors        int
	sumMs         float64
	minMs         float64
	maxMs         float64
	latenciesMs   []float64
	errorCounts   map[string]int
}

func newWSBenchRecorder(specs []wsSubscriptionSpec) *wsBenchRecorder {
	stats := make(map[string]*wsBenchSubscriptionStats, len(specs))
	for _, spec := range specs {
		stats[spec.Name] = &wsBenchSubscriptionStats{
			minMs:       -1,
			errorCounts: make(map[string]int),
		}
	}
	return &wsBenchRecorder{
		stats:        stats,
		globalErrors: make(map[string]int),
	}
}

func (r *wsBenchRecorder) Record(name string, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stat := r.stats[name]
	if stat == nil {
		r.globalErrors["unknown subscription "+name]++
		return
	}
	ms := durationMillis(latency)
	stat.notifications++
	stat.sumMs += ms
	stat.latenciesMs = append(stat.latenciesMs, ms)
	if stat.minMs < 0 || ms < stat.minMs {
		stat.minMs = ms
	}
	if ms > stat.maxMs {
		stat.maxMs = ms
	}
}

func (r *wsBenchRecorder) RecordError(name, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stat := r.stats[name]
	if stat == nil {
		r.globalErrors[truncateBenchmarkError(msg)]++
		return
	}
	stat.errors++
	stat.errorCounts[truncateBenchmarkError(msg)]++
}

func (r *wsBenchRecorder) RecordGlobalError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.globalErrors[truncateBenchmarkError(msg)]++
}

func (r *wsBenchRecorder) Reports(duration time.Duration, clients int) []wsSubscriptionReport {
	r.mu.Lock()
	snapshots := make(map[string]wsBenchSubscriptionStats, len(r.stats))
	for name, stat := range r.stats {
		snapshots[name] = wsBenchSubscriptionStats{
			notifications: stat.notifications,
			errors:        stat.errors,
			sumMs:         stat.sumMs,
			minMs:         stat.minMs,
			maxMs:         stat.maxMs,
			latenciesMs:   append([]float64(nil), stat.latenciesMs...),
			errorCounts:   copyIntMap(stat.errorCounts),
		}
	}
	r.mu.Unlock()

	reports := make([]wsSubscriptionReport, 0, len(snapshots))
	for name, stat := range snapshots {
		sort.Float64s(stat.latenciesMs)
		report := wsSubscriptionReport{
			Name:                      name,
			Clients:                   clients,
			Notifications:             stat.notifications,
			Errors:                    stat.errors,
			NotificationsPerSecond:    roundFloat(float64(stat.notifications) / duration.Seconds()),
			NotificationsPerClient:    roundFloat(float64(stat.notifications) / float64(clients)),
			NotificationsPerClientSec: roundFloat(float64(stat.notifications) / float64(clients) / duration.Seconds()),
			TopErrors:                 topBenchmarkErrors(stat.errorCounts, 4),
		}
		if stat.notifications > 0 {
			report.MinMs = roundFloat(stat.minMs)
			report.MeanMs = roundFloat(stat.sumMs / float64(stat.notifications))
			report.MaxMs = roundFloat(stat.maxMs)
			report.P50Ms = roundFloat(percentileValue(stat.latenciesMs, 0.50))
			report.P90Ms = roundFloat(percentileValue(stat.latenciesMs, 0.90))
			report.P95Ms = roundFloat(percentileValue(stat.latenciesMs, 0.95))
			report.P99Ms = roundFloat(percentileValue(stat.latenciesMs, 0.99))
			report.P999Ms = roundFloat(percentileValue(stat.latenciesMs, 0.999))
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].Name < reports[j].Name
	})
	return reports
}

func (r *wsBenchRecorder) GlobalErrors(limit int) []benchmarkErrorCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	return topBenchmarkErrors(copyIntMap(r.globalErrors), limit)
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type wsBenchmarkReport struct {
	Suite                  string                       `json:"suite"`
	GeneratedAt            string                       `json:"generated_at"`
	OutputDir              string                       `json:"output_dir"`
	WarmupDurationSeconds  float64                      `json:"warmup_duration_seconds"`
	MeasureDurationSeconds float64                      `json:"measure_duration_seconds"`
	Environment            wsBenchmarkEnvironmentReport `json:"environment"`
	Seed                   wsBenchmarkSeedReport        `json:"seed"`
	Gateway                wsBenchmarkGatewayReport     `json:"gateway"`
	SubscriptionStatuses   []wsSubscriptionStatus       `json:"subscription_statuses"`
	Subscriptions          []wsSubscriptionReport       `json:"subscriptions"`
	Totals                 wsSubscriptionTotals         `json:"totals"`
	GlobalErrors           []benchmarkErrorCount        `json:"global_errors,omitempty"`
}

type wsBenchmarkEnvironmentReport struct {
	SourceRPC              string             `json:"source_rpc"`
	CometRPC               string             `json:"comet_rpc"`
	GRPCAddr               string             `json:"grpc_addr"`
	ChainID                string             `json:"chain_id"`
	EthChainID             int64              `json:"eth_chain_id"`
	VirtualizeCosmosEvents bool               `json:"virtualize_cosmos_events"`
	NoFile                 benchmarkNoFileCap `json:"nofile"`
}

type wsBenchmarkSeedReport struct {
	StressDurationSeconds  float64                     `json:"stress_duration_seconds"`
	AccountsNum            int                         `json:"accounts_num"`
	TransactionsPerAccount int                         `json:"transactions_per_account"`
	RateTPS                float64                     `json:"rate_tps"`
	HeadBefore             int64                       `json:"head_before"`
	HeadAfter              int64                       `json:"head_after"`
	Workloads              []wsBenchmarkStressWorkload `json:"workloads"`
}

type wsBenchmarkGatewayReport struct {
	RPCURL              string             `json:"rpc_url"`
	WSURL               string             `json:"ws_url"`
	DataDir             string             `json:"data_dir"`
	LogPath             string             `json:"log_path"`
	SyncDurationSeconds float64            `json:"sync_duration_seconds"`
	StatusBefore        syncStatusResponse `json:"status_before"`
	StatusAfter         syncStatusResponse `json:"status_after"`
}

type wsSubscriptionStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type wsSubscriptionReport struct {
	Name                      string                `json:"name"`
	Clients                   int                   `json:"clients"`
	Notifications             int                   `json:"notifications"`
	Errors                    int                   `json:"errors"`
	NotificationsPerSecond    float64               `json:"notifications_per_second"`
	NotificationsPerClient    float64               `json:"notifications_per_client"`
	NotificationsPerClientSec float64               `json:"notifications_per_client_second"`
	MinMs                     float64               `json:"min_ms,omitempty"`
	MeanMs                    float64               `json:"mean_ms,omitempty"`
	MaxMs                     float64               `json:"max_ms,omitempty"`
	P50Ms                     float64               `json:"p50_ms,omitempty"`
	P90Ms                     float64               `json:"p90_ms,omitempty"`
	P95Ms                     float64               `json:"p95_ms,omitempty"`
	P99Ms                     float64               `json:"p99_ms,omitempty"`
	P999Ms                    float64               `json:"p99_9_ms,omitempty"`
	TopErrors                 []benchmarkErrorCount `json:"top_errors,omitempty"`
}

type wsSubscriptionTotals struct {
	Notifications          int     `json:"notifications"`
	Errors                 int     `json:"errors"`
	NotificationsPerSecond float64 `json:"notifications_per_second"`
}

func summarizeWSSubscriptionReports(reports []wsSubscriptionReport) wsSubscriptionTotals {
	var totals wsSubscriptionTotals
	for _, report := range reports {
		totals.Notifications += report.Notifications
		totals.Errors += report.Errors
		totals.NotificationsPerSecond += report.NotificationsPerSecond
	}
	totals.NotificationsPerSecond = roundFloat(totals.NotificationsPerSecond)
	return totals
}

func findWSSubscriptionReport(reports []wsSubscriptionReport, name string) *wsSubscriptionReport {
	for i := range reports {
		if reports[i].Name == name {
			return &reports[i]
		}
	}
	return nil
}

func writeWSBenchmarkReport(t *testing.T, artifactsDir string, report wsBenchmarkReport) {
	t.Helper()

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal ws benchmark report: %v", err)
	}
	jsonPath := filepath.Join(artifactsDir, "report.json")
	if err := os.WriteFile(jsonPath, reportJSON, 0o644); err != nil {
		t.Fatalf("write ws benchmark json report: %v", err)
	}
}

func getenvFloat64(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
