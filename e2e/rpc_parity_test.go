package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	defaultParityGatewayRPCPort = 8646
	defaultParityGatewayWSPort  = 8647
	defaultSeedDuration         = 2 * time.Second
	defaultSeedAccountsNum      = 24
	defaultInternalCallIters    = 200
	defaultSeedSettleDelay      = 2 * time.Second
)

func TestRPCParityAgainstLiveSource(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E") != "1" {
		t.Skip("set WEB3INJ_E2E=1 to run rpc parity e2e")
	}

	ctx := context.Background()
	sourceRPC := getenv("WEB3INJ_E2E_SOURCE_RPC", defaultSourceRPC)
	cometRPC := getenv("WEB3INJ_COMET_RPC", defaultCometRPC)
	grpcAddr := resolveParityGRPCAddr(t)
	chainID, err := cometChainID(ctx, cometRPC)
	if err != nil {
		t.Fatalf("query comet chain id: %v", err)
	}
	ethChainID, err := ethChainID(ctx, sourceRPC)
	if err != nil {
		t.Fatalf("query eth chain id: %v", err)
	}

	stresserRoot := getenv(
		"WEB3INJ_E2E_CHAIN_STRESSER_DIR",
		filepath.Clean(filepath.Join(projectRoot(t), "..", "chain-stresser")),
	)
	accountsPath := filepath.Join(stresserRoot, "chain-stresser-deploy", "instances", "0", "accounts.json")
	if _, err := os.Stat(accountsPath); err != nil {
		t.Fatalf("accounts file not found at %s: %v", accountsPath, err)
	}

	headBefore, err := ethBlockNumber(ctx, sourceRPC)
	if err != nil {
		t.Fatalf("query source head before generation: %v", err)
	}

	stresserBin := buildChainStresserBinary(t, stresserRoot)
	seedDuration := getenvDurationSeconds("WEB3INJ_E2E_SEED_DURATION_SEC", defaultSeedDuration)
	seedAccountsNum := getenvInt("WEB3INJ_E2E_SEED_ACCOUNTS_NUM", defaultSeedAccountsNum)
	internalCallIterations := getenvInt("WEB3INJ_E2E_INTERNAL_CALL_ITERATIONS", defaultInternalCallIters)
	seedSettleDelay := getenvDurationSeconds("WEB3INJ_E2E_SEED_SETTLE_SEC", defaultSeedSettleDelay)
	commonArgs := []string{
		"--chain-id", chainID,
		"--eth-chain-id", strconv.FormatInt(ethChainID, 10),
		"--node-addr", endpointHostPort(t, cometRPC),
		"--grpc-addr", grpcAddr,
		"--accounts", accountsPath,
		"--accounts-num", strconv.Itoa(seedAccountsNum),
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "eth_send", args: []string{"tx-eth-send"}},
		{name: "eth_call", args: []string{"tx-eth-call"}},
		{name: "eth_deploy", args: []string{"tx-eth-deploy"}},
		{name: "eth_internal_call", args: []string{"tx-eth-internal-call", "--iterations", strconv.Itoa(internalCallIterations)}},
	} {
		t.Run("generate_"+tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), commonArgs...)
			runChainStresserFor(t, stresserRoot, stresserBin, args, seedDuration)
		})
	}

	time.Sleep(seedSettleDelay)

	headAfter, err := ethBlockNumber(ctx, sourceRPC)
	if err != nil {
		t.Fatalf("query source head after generation: %v", err)
	}
	if headAfter <= headBefore {
		t.Fatalf("no new source blocks after traffic generation: before=%d after=%d", headBefore, headAfter)
	}

	generatedFrom := maxInt64(1, headBefore+1)
	txs, err := discoverTxCandidatesInRange(ctx, sourceRPC, generatedFrom, headAfter, 96)
	if err != nil {
		t.Fatalf("discover generated tx candidates: %v", err)
	}
	if len(txs) == 0 {
		t.Fatalf("no EVM tx candidates discovered in generated range [%d,%d]", generatedFrom, headAfter)
	}

	gatewayBin := buildGatewayBinary(t)
	dataDir := filepath.Join(t.TempDir(), "parity-gateway")
	proc := startGateway(t, gatewayStartConfig{
		BinaryPath:  gatewayBin,
		DataDir:     dataDir,
		RPCPort:     defaultParityGatewayRPCPort,
		WSPort:      defaultParityGatewayWSPort,
		Earliest:    generatedFrom,
		FetchJobs:   4,
		CometRPC:    cometRPC,
		GRPCAddr:    grpcAddr,
		ChainID:     chainID,
		EnableSync:  true,
		EnableRPC:   true,
		APIList:     "eth,net,web3,debug,inj",
		WaitTimeout: 90 * time.Second,
	})
	defer proc.Stop(t)

	waitForCondition(t, 4*time.Minute, func() (bool, error) {
		st, err := proc.Status(ctx)
		if err != nil {
			return false, err
		}
		if st.GapsRemaining != 0 || st.LastSyncedBlock < headAfter {
			return false, nil
		}

		srcHead, err := ethBlockNumber(ctx, sourceRPC)
		if err != nil {
			return false, err
		}
		dstHead, err := ethBlockNumber(ctx, proc.RPCURL())
		if err != nil {
			return false, err
		}
		return srcHead == dstHead && dstHead >= headAfter, nil
	}, "gateway did not fully sync and reach rpc parity with source head")

	compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_chainId", []interface{}{})
	compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_syncing", []interface{}{})
	compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_blockNumber", []interface{}{})

	blockHeights := sampleHeights(generatedFrom, headAfter, 8)
	var txBlockHeights []int64
	for _, height := range blockHeights {
		tag := fmt.Sprintf("0x%x", height)

		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockByNumber", []interface{}{tag, false})

		var block parityBlock
		if err := rpcCall(ctx, sourceRPC, "eth_getBlockByNumber", []interface{}{tag, false}, &block); err != nil {
			t.Fatalf("fetch source block %d: %v", height, err)
		}
		if block.Hash == "" {
			t.Fatalf("source block %d returned empty hash", height)
		}

		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockByHash", []interface{}{block.Hash, false})
		if len(block.Transactions) > 0 {
			compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockReceipts", []interface{}{tag})
			compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockReceipts", []interface{}{block.Hash})
			txBlockHeights = append(txBlockHeights, height)
		}
	}

	for _, height := range sampleInt64s(txBlockHeights, 4) {
		tag := fmt.Sprintf("0x%x", height)
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockByNumber", []interface{}{tag, true})

		var block parityBlock
		if err := rpcCall(ctx, sourceRPC, "eth_getBlockByNumber", []interface{}{tag, false}, &block); err != nil {
			t.Fatalf("fetch source block hash for full-tx comparison %d: %v", height, err)
		}
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getBlockByHash", []interface{}{block.Hash, true})
	}

	for _, height := range sampleInt64s(uniqueInt64s(append(blockHeights, txBlockHeights...)), 8) {
		filter := map[string]interface{}{
			"fromBlock": fmt.Sprintf("0x%x", height),
			"toBlock":   fmt.Sprintf("0x%x", height),
		}
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getLogs", []interface{}{filter})
	}

	var contractAddresses []string
	for _, tx := range sampleTxCandidates(txs, 12) {
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getTransactionByHash", []interface{}{tx.Hash})
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getTransactionReceipt", []interface{}{tx.Hash})

		var sourceTx parityTx
		if err := rpcCall(ctx, sourceRPC, "eth_getTransactionByHash", []interface{}{tx.Hash}, &sourceTx); err != nil {
			t.Fatalf("fetch source tx %s: %v", tx.Hash, err)
		}
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getTransactionByBlockNumberAndIndex", []interface{}{sourceTx.BlockNumber, sourceTx.TransactionIndex})
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getTransactionByBlockHashAndIndex", []interface{}{sourceTx.BlockHash, sourceTx.TransactionIndex})

		var receipt parityReceipt
		if err := rpcCall(ctx, sourceRPC, "eth_getTransactionReceipt", []interface{}{tx.Hash}, &receipt); err != nil {
			t.Fatalf("fetch source receipt %s: %v", tx.Hash, err)
		}
		if receipt.ContractAddress != nil && *receipt.ContractAddress != "" {
			contractAddresses = append(contractAddresses, *receipt.ContractAddress)
		}
	}

	for _, addr := range sampleStrings(uniqueStrings(contractAddresses), 3) {
		compareRPCParity(t, sourceRPC, proc.RPCURL(), "eth_getCode", []interface{}{addr, "latest"})
	}

	batchHeights := sampleHeights(generatedFrom, headAfter, 2)
	batchTxs := sampleTxCandidates(txs, 2)
	if len(batchHeights) > 0 && len(batchTxs) > 0 {
		blockTag := fmt.Sprintf("0x%x", batchHeights[0])
		logFilter := map[string]interface{}{
			"fromBlock": blockTag,
			"toBlock":   blockTag,
		}

		batchRequests := []rpcEnvelope{
			{JSONRPC: "2.0", ID: 101, Method: "eth_chainId", Params: []interface{}{}},
			{JSONRPC: "2.0", ID: 102, Method: "eth_getBlockByNumber", Params: []interface{}{blockTag, false}},
			{JSONRPC: "2.0", ID: 103, Method: "eth_getTransactionByHash", Params: []interface{}{batchTxs[0].Hash}},
			{JSONRPC: "2.0", ID: 104, Method: "eth_getTransactionReceipt", Params: []interface{}{batchTxs[0].Hash}},
			{JSONRPC: "2.0", ID: 105, Method: "eth_getLogs", Params: []interface{}{logFilter}},
		}
		compareRPCBatchParity(t, sourceRPC, proc.RPCURL(), batchRequests)
	}

	debugSourceRPC := resolveDebugSourceRPC(t, sourceRPC)
	state := prepareExpandedParityState(t, ctx, sourceRPC, accountsPath, ethChainID, seedAccountsNum, txs, contractAddresses)
	runExtendedNamespaceParity(t, sourceRPC, debugSourceRPC, proc.RPCURL(), state)
	runGatewayOnlyDebugCoverage(t, proc.RPCURL())

	st, err := proc.Status(ctx)
	if err != nil {
		t.Fatalf("query gateway sync status: %v", err)
	}
	if st.Cache.TxByHash.Hits == 0 {
		t.Fatalf("expected tx_by_hash cache hits after parity queries, status=%+v", st.Cache.TxByHash)
	}
	if st.Cache.TxByIndex.Hits == 0 {
		t.Fatalf("expected tx_by_index cache hits after parity queries, status=%+v", st.Cache.TxByIndex)
	}
	if st.Cache.ReceiptByHash.Hits == 0 {
		t.Fatalf("expected receipt_by_hash cache hits after parity queries, status=%+v", st.Cache.ReceiptByHash)
	}
	if st.Cache.BlockLogs.Hits == 0 {
		t.Fatalf("expected block_logs cache hits after parity queries, status=%+v", st.Cache.BlockLogs)
	}
	if st.Cache.TxByHash.LiveFallbacks != 0 || st.Cache.TxByIndex.LiveFallbacks != 0 ||
		st.Cache.ReceiptByHash.LiveFallbacks != 0 || st.Cache.BlockLogs.LiveFallbacks != 0 {
		t.Fatalf("expected zero cache live fallbacks for synced parity queries, cache=%+v", st.Cache)
	}
}

type parityBlock struct {
	Hash         string            `json:"hash"`
	Transactions []json.RawMessage `json:"transactions"`
}

type parityTx struct {
	Hash             string `json:"hash"`
	BlockHash        string `json:"blockHash"`
	BlockNumber      string `json:"blockNumber"`
	TransactionIndex string `json:"transactionIndex"`
}

type parityReceipt struct {
	ContractAddress *string `json:"contractAddress"`
}

type detailedParityTx struct {
	Hash                 string  `json:"hash"`
	BlockHash            string  `json:"blockHash"`
	BlockNumber          string  `json:"blockNumber"`
	TransactionIndex     string  `json:"transactionIndex"`
	From                 string  `json:"from"`
	To                   *string `json:"to"`
	Input                string  `json:"input"`
	Value                string  `json:"value"`
	Gas                  string  `json:"gas"`
	GasPrice             string  `json:"gasPrice"`
	Nonce                string  `json:"nonce"`
	MaxFeePerGas         *string `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *string `json:"maxPriorityFeePerGas"`
	Type                 string  `json:"type"`
}

type expandedParityState struct {
	SampleTx       detailedParityTx
	CallTx         detailedParityTx
	SampleBlockTag string
	SampleBlockNum uint64
	ContractAddr   string
	StorageKey     string
	CallArgs       map[string]interface{}
	FillArgs       map[string]interface{}
	InvalidRawTx   string
}

func prepareExpandedParityState(
	t *testing.T,
	ctx context.Context,
	sourceRPC string,
	accountsPath string,
	chainID int64,
	seedAccountsNum int,
	candidates []txCandidate,
	contractAddresses []string,
) expandedParityState {
	t.Helper()

	if len(candidates) == 0 {
		t.Fatal("need at least one tx candidate for expanded parity checks")
	}

	sampleTx := mustDetailedTx(t, ctx, sourceRPC, candidates[0].Hash)
	callTx := sampleTx
	for _, candidate := range sampleTxCandidates(candidates, 24) {
		tx := mustDetailedTx(t, ctx, sourceRPC, candidate.Hash)
		if tx.To != nil && tx.Input != "" && tx.Input != "0x" {
			callTx = tx
			break
		}
		if callTx.To == nil && tx.To != nil {
			callTx = tx
		}
	}
	if callTx.To == nil {
		t.Fatal("unable to find a traceable tx with a recipient")
	}

	contractAddr := ""
	if unique := uniqueStrings(contractAddresses); len(unique) > 0 {
		contractAddr = unique[0]
	} else {
		contractAddr = *callTx.To
	}

	callArgs := map[string]interface{}{
		"from": callTx.From,
		"to":   *callTx.To,
	}
	if callTx.Input != "" && callTx.Input != "0x" {
		callArgs["data"] = callTx.Input
	}
	if callTx.Value != "" && callTx.Value != "0x0" {
		callArgs["value"] = callTx.Value
	}

	stalePrivB64, staleAddr, staleNonce := findStaleNonceAccount(t, ctx, sourceRPC, accountsPath, seedAccountsNum)
	fillArgs := map[string]interface{}{
		"from":                 staleAddr.Hex(),
		"to":                   sampleTx.From,
		"gas":                  "0x5208",
		"maxFeePerGas":         "0x3b9aca00",
		"maxPriorityFeePerGas": "0x0",
		"value":                "0x0",
		"nonce":                fmt.Sprintf("0x%x", staleNonce),
		"chainId":              fmt.Sprintf("0x%x", chainID),
	}

	sampleBlockNum, err := hexutil.DecodeUint64(sampleTx.BlockNumber)
	if err != nil {
		t.Fatalf("decode sample block number %q: %v", sampleTx.BlockNumber, err)
	}

	return expandedParityState{
		SampleTx:       sampleTx,
		CallTx:         callTx,
		SampleBlockTag: sampleTx.BlockNumber,
		SampleBlockNum: sampleBlockNum,
		ContractAddr:   contractAddr,
		StorageKey:     "0x" + strings.Repeat("0", 64),
		CallArgs:       callArgs,
		FillArgs:       fillArgs,
		InvalidRawTx:   buildSignedLegacyRawTx(t, stalePrivB64, chainID, staleNonce-1, common.HexToAddress(sampleTx.From)),
	}
}

func runExtendedNamespaceParity(
	t *testing.T,
	sourceRPC string,
	debugSourceRPC string,
	gatewayRPC string,
	state expandedParityState,
) {
	t.Helper()

	compareClientVersionSemantics(t, sourceRPC, gatewayRPC)

	for _, tc := range []struct {
		method string
		params []interface{}
	}{
		{method: "net_version", params: []interface{}{}},
		{method: "net_listening", params: []interface{}{}},
		{method: "net_peerCount", params: []interface{}{}},
		{method: "web3_sha3", params: []interface{}{"hello parity"}},
		{method: "eth_protocolVersion", params: []interface{}{}},
		{method: "eth_getBlockTransactionCountByHash", params: []interface{}{state.SampleTx.BlockHash}},
		{method: "eth_getBlockTransactionCountByNumber", params: []interface{}{state.SampleBlockTag}},
		{method: "eth_getBlockReceipts", params: []interface{}{state.SampleBlockTag}},
		{method: "eth_getBlockReceipts", params: []interface{}{state.SampleTx.BlockHash}},
		{method: "eth_getBalance", params: []interface{}{state.SampleTx.From, state.SampleBlockTag}},
		{method: "eth_getTransactionCount", params: []interface{}{state.SampleTx.From, state.SampleBlockTag}},
		{method: "eth_getStorageAt", params: []interface{}{state.ContractAddr, state.StorageKey, "latest"}},
		{method: "eth_getProof", params: []interface{}{state.SampleTx.From, []interface{}{state.StorageKey}, state.SampleBlockTag}},
		{method: "eth_call", params: []interface{}{state.CallArgs, "latest"}},
		{method: "eth_gasPrice", params: []interface{}{}},
		{method: "eth_estimateGas", params: []interface{}{state.CallArgs, "latest"}},
		{method: "eth_feeHistory", params: []interface{}{"0x2", state.SampleBlockTag, []interface{}{25.0, 50.0}}},
		{method: "eth_maxPriorityFeePerGas", params: []interface{}{}},
		{method: "eth_getUncleByBlockHashAndIndex", params: []interface{}{state.SampleTx.BlockHash, "0x0"}},
		{method: "eth_getUncleByBlockNumberAndIndex", params: []interface{}{state.SampleBlockTag, "0x0"}},
		{method: "eth_getUncleCountByBlockHash", params: []interface{}{state.SampleTx.BlockHash}},
		{method: "eth_getUncleCountByBlockNumber", params: []interface{}{state.SampleBlockTag}},
		{method: "eth_hashrate", params: []interface{}{}},
		{method: "eth_mining", params: []interface{}{}},
		{method: "eth_coinbase", params: []interface{}{}},
		{method: "eth_getTransactionLogs", params: []interface{}{state.SampleTx.Hash}},
		{method: "eth_fillTransaction", params: []interface{}{state.FillArgs}},
		{method: "eth_getPendingTransactions", params: []interface{}{}},
		{method: "eth_sendRawTransaction", params: []interface{}{state.InvalidRawTx}},
	} {
		compareRPCResponseParity(t, sourceRPC, gatewayRPC, tc.method, tc.params)
	}

	for _, tc := range []struct {
		method string
		params []interface{}
	}{
		{method: "debug_traceTransaction", params: []interface{}{state.SampleTx.Hash, map[string]interface{}{"tracer": "callTracer"}}},
		{method: "debug_traceBlockByNumber", params: []interface{}{state.SampleBlockTag, map[string]interface{}{"tracer": "callTracer"}}},
		{method: "debug_traceBlockByHash", params: []interface{}{state.SampleTx.BlockHash, map[string]interface{}{"tracer": "callTracer"}}},
		{method: "debug_traceCall", params: []interface{}{state.CallArgs, "latest", map[string]interface{}{"tracer": "callTracer"}}},
		{method: "debug_getHeaderRlp", params: []interface{}{state.SampleBlockTag}},
		{method: "debug_getBlockRlp", params: []interface{}{state.SampleBlockTag}},
		{method: "debug_printBlock", params: []interface{}{state.SampleBlockTag}},
		{method: "debug_intermediateRoots", params: []interface{}{state.SampleTx.Hash, map[string]interface{}{}}},
	} {
		compareRPCResponseParity(t, debugSourceRPC, gatewayRPC, tc.method, tc.params)
	}

	compareFilterMethodCoverage(t, sourceRPC, gatewayRPC, state)

	commonBatch := []rpcEnvelope{
		{JSONRPC: "2.0", ID: 201, Method: "net_version", Params: []interface{}{}},
		{JSONRPC: "2.0", ID: 202, Method: "net_listening", Params: []interface{}{}},
		{JSONRPC: "2.0", ID: 203, Method: "web3_sha3", Params: []interface{}{"hello parity"}},
		{JSONRPC: "2.0", ID: 204, Method: "eth_protocolVersion", Params: []interface{}{}},
		{JSONRPC: "2.0", ID: 205, Method: "eth_getBalance", Params: []interface{}{state.SampleTx.From, state.SampleBlockTag}},
		{JSONRPC: "2.0", ID: 206, Method: "eth_getTransactionCount", Params: []interface{}{state.SampleTx.From, state.SampleBlockTag}},
		{JSONRPC: "2.0", ID: 207, Method: "eth_getTransactionLogs", Params: []interface{}{state.SampleTx.Hash}},
		{JSONRPC: "2.0", ID: 208, Method: "eth_getPendingTransactions", Params: []interface{}{}},
	}
	compareRPCBatchParity(t, sourceRPC, gatewayRPC, commonBatch)

	debugBatch := []rpcEnvelope{
		{JSONRPC: "2.0", ID: 301, Method: "debug_traceTransaction", Params: []interface{}{state.SampleTx.Hash, map[string]interface{}{"tracer": "callTracer"}}},
		{JSONRPC: "2.0", ID: 302, Method: "debug_getHeaderRlp", Params: []interface{}{state.SampleBlockTag}},
		{JSONRPC: "2.0", ID: 303, Method: "debug_getBlockRlp", Params: []interface{}{state.SampleBlockTag}},
	}
	compareRPCBatchParity(t, debugSourceRPC, gatewayRPC, debugBatch)
}

func runGatewayOnlyDebugCoverage(t *testing.T, gatewayRPC string) {
	t.Helper()

	debugDir := t.TempDir()

	var gcStats map[string]interface{}
	if err := rpcCall(context.Background(), gatewayRPC, "debug_gcStats", []interface{}{}, &gcStats); err != nil {
		t.Fatalf("gateway debug_gcStats failed: %v", err)
	}
	if len(gcStats) == 0 {
		t.Fatal("gateway debug_gcStats returned empty object")
	}

	var memStats map[string]interface{}
	if err := rpcCall(context.Background(), gatewayRPC, "debug_memStats", []interface{}{}, &memStats); err != nil {
		t.Fatalf("gateway debug_memStats failed: %v", err)
	}
	if len(memStats) == 0 {
		t.Fatal("gateway debug_memStats returned empty object")
	}

	var stacks string
	if err := rpcCall(context.Background(), gatewayRPC, "debug_stacks", []interface{}{}, &stacks); err != nil {
		t.Fatalf("gateway debug_stacks failed: %v", err)
	}
	if !strings.Contains(stacks, "goroutine") {
		t.Fatalf("gateway debug_stacks returned unexpected content: %q", stacks)
	}

	var prevGCPercent int
	if err := rpcCall(context.Background(), gatewayRPC, "debug_setGCPercent", []interface{}{100}, &prevGCPercent); err != nil {
		t.Fatalf("gateway debug_setGCPercent failed: %v", err)
	}
	expectRPCSuccess(t, gatewayRPC, "debug_setGCPercent", []interface{}{prevGCPercent})
	expectRPCSuccess(t, gatewayRPC, "debug_setBlockProfileRate", []interface{}{1})
	expectRPCSuccess(t, gatewayRPC, "debug_setBlockProfileRate", []interface{}{0})
	expectRPCSuccess(t, gatewayRPC, "debug_setMutexProfileFraction", []interface{}{1})
	expectRPCSuccess(t, gatewayRPC, "debug_setMutexProfileFraction", []interface{}{0})
	expectRPCSuccess(t, gatewayRPC, "debug_freeOSMemory", []interface{}{})

	writeProfilePath := func(name string) string {
		return filepath.Join(debugDir, name)
	}

	for _, tc := range []struct {
		method string
		params []interface{}
		path   string
	}{
		{method: "debug_writeBlockProfile", params: []interface{}{writeProfilePath("block.pprof")}, path: writeProfilePath("block.pprof")},
		{method: "debug_writeMemProfile", params: []interface{}{writeProfilePath("heap.pprof")}, path: writeProfilePath("heap.pprof")},
		{method: "debug_writeMutexProfile", params: []interface{}{writeProfilePath("mutex.pprof")}, path: writeProfilePath("mutex.pprof")},
		{method: "debug_blockProfile", params: []interface{}{writeProfilePath("block_timed.pprof"), 1}, path: writeProfilePath("block_timed.pprof")},
		{method: "debug_mutexProfile", params: []interface{}{writeProfilePath("mutex_timed.pprof"), 1}, path: writeProfilePath("mutex_timed.pprof")},
		{method: "debug_cPUProfile", params: []interface{}{writeProfilePath("cpu.pprof"), 1}, path: writeProfilePath("cpu.pprof")},
		{method: "debug_goTrace", params: []interface{}{writeProfilePath("trace.out"), 1}, path: writeProfilePath("trace.out")},
	} {
		expectRPCSuccess(t, gatewayRPC, tc.method, tc.params)
		assertFileWritten(t, tc.path)
	}

	cpuProfilePath := writeProfilePath("cpu_manual.pprof")
	expectRPCSuccess(t, gatewayRPC, "debug_startCPUProfile", []interface{}{cpuProfilePath})
	time.Sleep(200 * time.Millisecond)
	expectRPCSuccess(t, gatewayRPC, "debug_stopCPUProfile", []interface{}{})
	assertFileWritten(t, cpuProfilePath)

	goTracePath := writeProfilePath("trace_manual.out")
	expectRPCSuccess(t, gatewayRPC, "debug_startGoTrace", []interface{}{goTracePath})
	time.Sleep(200 * time.Millisecond)
	expectRPCSuccess(t, gatewayRPC, "debug_stopGoTrace", []interface{}{})
	assertFileWritten(t, goTracePath)
}

func assertFileWritten(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected debug artifact %s to exist: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("expected debug artifact %s to be non-empty", path)
	}
}

func expectRPCSuccess(t *testing.T, rpcURL, method string, params interface{}) {
	t.Helper()

	resp := mustRPCCallResponse(t, rpcURL, method, params)
	if resp.Error != nil {
		t.Fatalf("%s returned rpc error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
}

func compareFilterMethodCoverage(t *testing.T, sourceRPC, gatewayRPC string, state expandedParityState) {
	t.Helper()

	logFilter := map[string]interface{}{
		"fromBlock": state.SampleBlockTag,
		"toBlock":   state.SampleBlockTag,
	}
	sourceLogFilterID := mustRPCStringResult(t, sourceRPC, "eth_newFilter", []interface{}{logFilter})
	gatewayLogFilterID := mustRPCStringResult(t, gatewayRPC, "eth_newFilter", []interface{}{logFilter})
	if sourceLogFilterID == "" || gatewayLogFilterID == "" {
		t.Fatalf("expected non-empty log filter ids, source=%q gateway=%q", sourceLogFilterID, gatewayLogFilterID)
	}
	compareResponsePair(
		t,
		"eth_getFilterLogs",
		mustRPCCallResponse(t, sourceRPC, "eth_getFilterLogs", []interface{}{sourceLogFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_getFilterLogs", []interface{}{gatewayLogFilterID}),
	)
	compareResponsePair(
		t,
		"eth_getFilterChanges(log)",
		mustRPCCallResponse(t, sourceRPC, "eth_getFilterChanges", []interface{}{sourceLogFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_getFilterChanges", []interface{}{gatewayLogFilterID}),
	)
	compareResponsePair(
		t,
		"eth_uninstallFilter(log)",
		mustRPCCallResponse(t, sourceRPC, "eth_uninstallFilter", []interface{}{sourceLogFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_uninstallFilter", []interface{}{gatewayLogFilterID}),
	)

	sourcePendingFilterID := mustRPCStringResult(t, sourceRPC, "eth_newPendingTransactionFilter", []interface{}{})
	gatewayPendingFilterID := mustRPCStringResult(t, gatewayRPC, "eth_newPendingTransactionFilter", []interface{}{})
	if sourcePendingFilterID == "" || gatewayPendingFilterID == "" {
		t.Fatalf("expected non-empty pending filter ids, source=%q gateway=%q", sourcePendingFilterID, gatewayPendingFilterID)
	}
	compareResponsePair(
		t,
		"eth_getFilterChanges(pending)",
		mustRPCCallResponse(t, sourceRPC, "eth_getFilterChanges", []interface{}{sourcePendingFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_getFilterChanges", []interface{}{gatewayPendingFilterID}),
	)
	compareResponsePair(
		t,
		"eth_uninstallFilter(pending)",
		mustRPCCallResponse(t, sourceRPC, "eth_uninstallFilter", []interface{}{sourcePendingFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_uninstallFilter", []interface{}{gatewayPendingFilterID}),
	)

	sourceBlockFilterID := mustRPCStringResult(t, sourceRPC, "eth_newBlockFilter", []interface{}{})
	gatewayBlockFilterID := mustRPCStringResult(t, gatewayRPC, "eth_newBlockFilter", []interface{}{})
	if sourceBlockFilterID == "" || gatewayBlockFilterID == "" {
		t.Fatalf("expected non-empty block filter ids, source=%q gateway=%q", sourceBlockFilterID, gatewayBlockFilterID)
	}
	compareResponsePair(
		t,
		"eth_getFilterChanges(block)",
		mustRPCCallResponse(t, sourceRPC, "eth_getFilterChanges", []interface{}{sourceBlockFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_getFilterChanges", []interface{}{gatewayBlockFilterID}),
	)
	compareResponsePair(
		t,
		"eth_uninstallFilter(block)",
		mustRPCCallResponse(t, sourceRPC, "eth_uninstallFilter", []interface{}{sourceBlockFilterID}),
		mustRPCCallResponse(t, gatewayRPC, "eth_uninstallFilter", []interface{}{gatewayBlockFilterID}),
	)
}

func resolveDebugSourceRPC(t *testing.T, sourceRPC string) string {
	t.Helper()

	resp := mustRPCCallResponse(t, sourceRPC, "debug_traceTransaction", []interface{}{
		"0x0000000000000000000000000000000000000000000000000000000000000000",
		map[string]interface{}{"tracer": "callTracer"},
	})
	if resp.Error == nil || resp.Error.Code != -32601 {
		return sourceRPC
	}

	fallback := replaceURLPort(t, sourceRPC, "8547")
	resp = mustRPCCallResponse(t, fallback, "debug_traceTransaction", []interface{}{
		"0x0000000000000000000000000000000000000000000000000000000000000000",
		map[string]interface{}{"tracer": "callTracer"},
	})
	if resp.Error != nil && resp.Error.Code == -32601 {
		t.Fatalf("debug namespace unavailable on both %s and %s", sourceRPC, fallback)
	}
	return fallback
}

func replaceURLPort(t *testing.T, rawURL, port string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse rpc url %q: %v", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		t.Fatalf("rpc url %q has empty host", rawURL)
	}
	u.Host = net.JoinHostPort(host, port)
	return u.String()
}

func compareClientVersionSemantics(t *testing.T, sourceRPC, gatewayRPC string) {
	t.Helper()

	var sourceVersion string
	if err := rpcCall(context.Background(), sourceRPC, "web3_clientVersion", []interface{}{}, &sourceVersion); err != nil {
		t.Fatalf("source web3_clientVersion failed: %v", err)
	}
	var gatewayVersion string
	if err := rpcCall(context.Background(), gatewayRPC, "web3_clientVersion", []interface{}{}, &gatewayVersion); err != nil {
		t.Fatalf("gateway web3_clientVersion failed: %v", err)
	}
	if strings.TrimSpace(sourceVersion) == "" {
		t.Fatal("source web3_clientVersion returned empty string")
	}
	if !strings.HasPrefix(gatewayVersion, "evm-gateway/") {
		t.Fatalf("gateway web3_clientVersion should identify the service, got %q", gatewayVersion)
	}
}

func mustDetailedTx(t *testing.T, ctx context.Context, rpcURL, hash string) detailedParityTx {
	t.Helper()

	var tx detailedParityTx
	if err := rpcCall(ctx, rpcURL, "eth_getTransactionByHash", []interface{}{hash}, &tx); err != nil {
		t.Fatalf("fetch detailed tx %s: %v", hash, err)
	}
	if strings.TrimSpace(tx.Hash) == "" {
		t.Fatalf("detailed tx %s returned empty hash", hash)
	}
	return tx
}

func findStaleNonceAccount(t *testing.T, ctx context.Context, sourceRPC, accountsPath string, startIdx int) (string, common.Address, uint64) {
	t.Helper()

	keys := readAccountPrivateKeys(t, accountsPath)
	scanOrder := make([]int, 0, len(keys))
	for idx := startIdx; idx < len(keys); idx++ {
		scanOrder = append(scanOrder, idx)
	}
	for idx := 0; idx < startIdx && idx < len(keys); idx++ {
		scanOrder = append(scanOrder, idx)
	}
	for _, idx := range scanOrder {
		addr := addressFromBase64Key(t, keys[idx])
		var nonceHex string
		if err := rpcCall(ctx, sourceRPC, "eth_getTransactionCount", []interface{}{addr.Hex(), "latest"}, &nonceHex); err != nil {
			t.Fatalf("query nonce for %s: %v", addr.Hex(), err)
		}
		nonce, err := hexutil.DecodeUint64(nonceHex)
		if err != nil {
			t.Fatalf("decode nonce %q for %s: %v", nonceHex, addr.Hex(), err)
		}
		if nonce > 0 {
			return keys[idx], addr, nonce
		}
	}

	t.Fatalf("unable to find account with stale nonce candidate starting from index %d", startIdx)
	return "", common.Address{}, 0
}

func readAccountPrivateKeys(t *testing.T, accountsPath string) []string {
	t.Helper()

	bz, err := os.ReadFile(accountsPath)
	if err != nil {
		t.Fatalf("read accounts file %s: %v", accountsPath, err)
	}
	var out []string
	if err := json.Unmarshal(bz, &out); err != nil {
		t.Fatalf("decode accounts file %s: %v", accountsPath, err)
	}
	return out
}

func addressFromBase64Key(t *testing.T, encoded string) common.Address {
	t.Helper()

	keyBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64 private key: %v", err)
	}
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	return crypto.PubkeyToAddress(key.PublicKey)
}

func buildSignedLegacyRawTx(t *testing.T, encodedKey string, chainID int64, nonce uint64, to common.Address) string {
	t.Helper()

	keyBytes, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		t.Fatalf("decode base64 private key: %v", err)
	}
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}

	tx := ethtypes.NewTx(&ethtypes.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Gas:      21_000,
		GasPrice: big.NewInt(1_000_000_000),
		Value:    big.NewInt(0),
	})
	signed, err := ethtypes.SignTx(tx, ethtypes.LatestSignerForChainID(big.NewInt(chainID)), key)
	if err != nil {
		t.Fatalf("sign legacy tx: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal signed tx: %v", err)
	}
	return hexutil.Encode(raw)
}

func buildChainStresserBinary(t *testing.T, stresserRoot string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "chain-stresser-e2e")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/chain-stresser")
	cmd.Dir = stresserRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build chain-stresser failed: %v\n%s", err, string(output))
	}
	return binPath
}

func runChainStresserFor(t *testing.T, stresserRoot, binPath string, args []string, duration time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = stresserRoot
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Logf("stopped chain-stresser after %s: %s", duration, strings.Join(args, " "))
		return
	}
	if err != nil {
		t.Fatalf("chain-stresser failed: %v\ncommand: %s\n%s", err, strings.Join(append([]string{binPath}, args...), " "), string(output))
	}
}

func resolveParityGRPCAddr(t *testing.T) string {
	t.Helper()

	if v := strings.TrimSpace(os.Getenv("WEB3INJ_GRPC_ADDR")); v != "" {
		return v
	}

	for _, candidate := range []string{"127.0.0.1:9900", "127.0.0.1:9090"} {
		if canDialTCP(candidate, 750*time.Millisecond) {
			return candidate
		}
	}

	t.Fatalf("unable to resolve gRPC address; set WEB3INJ_GRPC_ADDR explicitly")
	return ""
}

func canDialTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func endpointHostPort(t *testing.T, endpoint string) string {
	t.Helper()

	if !strings.Contains(endpoint, "://") {
		return strings.TrimSpace(endpoint)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	if strings.TrimSpace(u.Host) == "" {
		t.Fatalf("endpoint %q has empty host", endpoint)
	}
	return u.Host
}

func ethChainID(ctx context.Context, rpcURL string) (int64, error) {
	var hexChainID string
	if err := rpcCall(ctx, rpcURL, "eth_chainId", []interface{}{}, &hexChainID); err != nil {
		return 0, err
	}
	return hexToInt64(hexChainID)
}

func discoverTxCandidatesInRange(ctx context.Context, rpcURL string, start, end int64, limit int) ([]txCandidate, error) {
	out := make([]txCandidate, 0, limit)
	seen := make(map[string]struct{})

	for height := start; height <= end; height++ {
		hashes, err := ethBlockTxHashes(ctx, rpcURL, height)
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
			if len(out) >= limit {
				return out, nil
			}
		}
	}

	return out, nil
}

func compareRPCParity(t *testing.T, sourceRPC, gatewayRPC, method string, params []interface{}) {
	t.Helper()

	sourceRaw, err := rpcRawResult(context.Background(), sourceRPC, method, params)
	if err != nil {
		t.Fatalf("source %s failed: %v", method, err)
	}
	gatewayRaw, err := rpcRawResult(context.Background(), gatewayRPC, method, params)
	if err != nil {
		t.Fatalf("gateway %s failed: %v", method, err)
	}

	sourceNorm, err := normalizeJSON(sourceRaw)
	if err != nil {
		t.Fatalf("normalize source %s result: %v", method, err)
	}
	gatewayNorm, err := normalizeJSON(gatewayRaw)
	if err != nil {
		t.Fatalf("normalize gateway %s result: %v", method, err)
	}

	if !bytes.Equal(sourceNorm, gatewayNorm) {
		t.Fatalf(
			"rpc parity mismatch for %s\nparams: %s\nsource:  %s\ngateway: %s",
			method,
			mustPrettyJSON(t, mustJSONMarshal(t, params)),
			mustPrettyJSON(t, sourceNorm),
			mustPrettyJSON(t, gatewayNorm),
		)
	}
}

func compareRPCResponseParity(t *testing.T, sourceRPC, gatewayRPC, method string, params []interface{}) {
	t.Helper()

	compareResponsePair(
		t,
		method,
		mustRPCCallResponse(t, sourceRPC, method, params),
		mustRPCCallResponse(t, gatewayRPC, method, params),
	)
}

func compareResponsePair(t *testing.T, label string, sourceResp, gatewayResp rpcResponse) {
	t.Helper()

	sourceNorm, err := normalizeRPCResponse(sourceResp)
	if err != nil {
		t.Fatalf("normalize source response for %s: %v", label, err)
	}
	gatewayNorm, err := normalizeRPCResponse(gatewayResp)
	if err != nil {
		t.Fatalf("normalize gateway response for %s: %v", label, err)
	}
	if !bytes.Equal(sourceNorm, gatewayNorm) {
		t.Fatalf(
			"rpc parity mismatch for %s\nsource:  %s\ngateway: %s",
			label,
			mustPrettyJSON(t, sourceNorm),
			mustPrettyJSON(t, gatewayNorm),
		)
	}
}

func compareRPCBatchParity(t *testing.T, sourceRPC, gatewayRPC string, requests []rpcEnvelope) {
	t.Helper()

	sourceResponses, err := rpcBatchCall(context.Background(), sourceRPC, requests)
	if err != nil {
		t.Fatalf("source batch call failed: %v", err)
	}
	gatewayResponses, err := rpcBatchCall(context.Background(), gatewayRPC, requests)
	if err != nil {
		t.Fatalf("gateway batch call failed: %v", err)
	}

	sourceNorm, err := normalizeBatchResponses(sourceResponses)
	if err != nil {
		t.Fatalf("normalize source batch: %v", err)
	}
	gatewayNorm, err := normalizeBatchResponses(gatewayResponses)
	if err != nil {
		t.Fatalf("normalize gateway batch: %v", err)
	}

	if !bytes.Equal(sourceNorm, gatewayNorm) {
		t.Fatalf(
			"batch rpc parity mismatch\nrequests: %s\nsource:  %s\ngateway: %s",
			mustPrettyJSON(t, mustJSONMarshal(t, requests)),
			mustPrettyJSON(t, sourceNorm),
			mustPrettyJSON(t, gatewayNorm),
		)
	}
}

func mustRPCCallResponse(t *testing.T, rpcURL, method string, params interface{}) rpcResponse {
	t.Helper()
	resp, err := rpcCallResponse(context.Background(), rpcURL, method, params)
	if err != nil {
		t.Fatalf("%s call to %s failed: %v", method, rpcURL, err)
	}
	return resp
}

func rpcRawResult(ctx context.Context, rpcURL, method string, params interface{}) (json.RawMessage, error) {
	resp, err := rpcCallResponse(ctx, rpcURL, method, params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

func rpcCallResponse(ctx context.Context, rpcURL, method string, params interface{}) (rpcResponse, error) {
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rpcResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return rpcResponse{}, fmt.Errorf("rpc http status %d: %s", resp.StatusCode, string(b))
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return rpcResponse{}, err
	}
	return rpcResp, nil
}

func rpcBatchCall(ctx context.Context, rpcURL string, requests []rpcEnvelope) ([]rpcResponse, error) {
	reqBody, err := json.Marshal(requests)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("rpc http status %d: %s", resp.StatusCode, string(b))
	}

	var rpcResp []rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	return rpcResp, nil
}

func mustRPCStringResult(t *testing.T, rpcURL, method string, params interface{}) string {
	t.Helper()

	var out string
	if err := rpcCall(context.Background(), rpcURL, method, params, &out); err != nil {
		t.Fatalf("%s call to %s failed: %v", method, rpcURL, err)
	}
	return out
}

func normalizeJSON(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte("null"), nil
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()

	var v interface{}
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func normalizeRPCResponse(resp rpcResponse) ([]byte, error) {
	type canonicalRPCResponse struct {
		Result interface{} `json:"result,omitempty"`
		Error  *rpcError   `json:"error,omitempty"`
	}

	entry := canonicalRPCResponse{
		Error: resp.Error,
	}
	if resp.Result != nil {
		decoder := json.NewDecoder(bytes.NewReader(resp.Result))
		decoder.UseNumber()

		var v interface{}
		if err := decoder.Decode(&v); err != nil {
			return nil, err
		}
		entry.Result = v
	}

	return json.Marshal(entry)
}

func normalizeBatchResponses(responses []rpcResponse) ([]byte, error) {
	type canonicalRPCResponse struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      int         `json:"id"`
		Result  interface{} `json:"result,omitempty"`
		Error   *rpcError   `json:"error,omitempty"`
	}

	canon := make([]canonicalRPCResponse, 0, len(responses))
	for _, resp := range responses {
		entry := canonicalRPCResponse{
			JSONRPC: resp.JSONRPC,
			ID:      resp.ID,
			Error:   resp.Error,
		}
		if resp.Result != nil {
			decoder := json.NewDecoder(bytes.NewReader(resp.Result))
			decoder.UseNumber()

			var v interface{}
			if err := decoder.Decode(&v); err != nil {
				return nil, err
			}
			entry.Result = v
		}
		canon = append(canon, entry)
	}

	slices.SortFunc(canon, func(a, b canonicalRPCResponse) int {
		return a.ID - b.ID
	})
	return json.Marshal(canon)
}

func mustPrettyJSON(t *testing.T, data []byte) string {
	t.Helper()

	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}
	return out.String()
}

func mustJSONMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()

	bz, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	return bz
}

func sampleHeights(start, end int64, limit int) []int64 {
	if end < start {
		return nil
	}

	all := make([]int64, 0, end-start+1)
	for height := start; height <= end; height++ {
		all = append(all, height)
	}
	return sampleInt64s(all, limit)
}

func sampleInt64s(values []int64, limit int) []int64 {
	if len(values) <= limit {
		return append([]int64(nil), values...)
	}
	if limit <= 1 {
		return []int64{values[0]}
	}

	out := make([]int64, 0, limit)
	last := len(values) - 1
	for i := 0; i < limit; i++ {
		idx := i * last / (limit - 1)
		out = append(out, values[idx])
	}
	return uniqueInt64s(out)
}

func sampleTxCandidates(values []txCandidate, limit int) []txCandidate {
	if len(values) <= limit {
		return append([]txCandidate(nil), values...)
	}
	if limit <= 1 {
		return []txCandidate{values[0]}
	}

	out := make([]txCandidate, 0, limit)
	last := len(values) - 1
	for i := 0; i < limit; i++ {
		idx := i * last / (limit - 1)
		out = append(out, values[idx])
	}
	return out
}

func sampleStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return append([]string(nil), values...)
	}
	if limit <= 1 {
		return []string{values[0]}
	}

	out := make([]string, 0, limit)
	last := len(values) - 1
	for i := 0; i < limit; i++ {
		idx := i * last / (limit - 1)
		out = append(out, values[idx])
	}
	return uniqueStrings(out)
}

func uniqueInt64s(values []int64) []int64 {
	out := make([]int64, 0, len(values))
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
