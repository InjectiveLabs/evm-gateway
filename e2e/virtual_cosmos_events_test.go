package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
)

const (
	defaultVirtualGatewayRPCPort = 8846
	defaultVirtualGatewayWSPort  = 8847
)

func TestCosmosEventVirtualizationAgainstLiveChain(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E") != "1" {
		t.Skip("set WEB3INJ_E2E=1 to run cosmos event virtualization e2e")
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
		"--min-gas-price", getenv("WEB3INJ_E2E_MIN_GAS_PRICE", "160000000inj"),
		"--transactions", "3",
		"--rate-tps", "25",
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "bank_send", args: []string{"tx-bank-send"}},
		{name: "bank_multi_send", args: []string{"tx-bank-send-many", "--targets", "3"}},
		{name: "tokenfactory_burn", args: []string{"tx-tokenfactory-burn"}},
		{name: "eth_send", args: []string{"tx-eth-send"}},
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

	gatewayBin := buildGatewayBinary(t)
	proc := startGateway(t, gatewayStartConfig{
		BinaryPath:             gatewayBin,
		DataDir:                filepath.Join(t.TempDir(), "virtual-cosmos-events-gateway"),
		RPCPort:                defaultVirtualGatewayRPCPort,
		WSPort:                 defaultVirtualGatewayWSPort,
		Earliest:               generatedFrom,
		FetchJobs:              4,
		CometRPC:               cometRPC,
		GRPCAddr:               grpcAddr,
		ChainID:                chainID,
		EVMChainID:             strconv.FormatInt(ethChainID, 10),
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
		dstHead, err := ethBlockNumber(ctx, proc.RPCURL())
		if err != nil {
			return false, err
		}
		return st.GapsRemaining == 0 && dstHead >= headAfter, nil
	}, "virtualized gateway did not sync generated range")

	virtualLogs := collectVirtualLogs(t, ctx, proc.RPCURL(), generatedFrom, headAfter)
	if len(virtualLogs) == 0 {
		t.Fatalf("expected virtual logs in generated range [%d,%d]", generatedFrom, headAfter)
	}

	topics := map[string]int{}
	var cosmosLog *virtualizedLog
	for i := range virtualLogs {
		log := &virtualLogs[i]
		if !strings.EqualFold(log.Address, virtualbank.ContractAddress.Hex()) {
			t.Fatalf("virtual log emitted from unexpected address: got %s want %s", log.Address, virtualbank.ContractAddress.Hex())
		}
		if !log.Virtual {
			t.Fatalf("virtual log missing virtual=true metadata: %#v", log)
		}
		if len(log.Topics) > 0 {
			topics[strings.ToLower(log.Topics[0])]++
		}
		if log.CosmosHash != nil && cosmosLog == nil {
			cosmosLog = log
		}
	}

	requireTopic := func(topic string) {
		t.Helper()
		if topics[strings.ToLower(topic)] == 0 {
			t.Fatalf("expected virtual logs to include topic %s, got topics=%v", topic, topics)
		}
	}
	requireTopic(virtualbank.TopicTransfer.Hex())
	requireTopic(virtualbank.TopicCoinSpent.Hex())
	requireTopic(virtualbank.TopicCoinReceived.Hex())
	requireTopic(virtualbank.TopicCoinbase.Hex())
	requireTopic(virtualbank.TopicBurn.Hex())

	if cosmosLog == nil {
		t.Fatal("expected at least one virtualized Cosmos tx log with cosmos_hash")
	}
	cosmosTx := fetchVirtualTx(t, ctx, proc.RPCURL(), cosmosLog.TransactionHash)
	if !cosmosTx.Virtual {
		t.Fatalf("expected tx %s to be virtual", cosmosLog.TransactionHash)
	}
	if cosmosTx.CosmosHash == nil || !strings.EqualFold(*cosmosTx.CosmosHash, *cosmosLog.CosmosHash) {
		t.Fatalf("cosmos_hash mismatch between tx and log: tx=%v log=%v", cosmosTx.CosmosHash, cosmosLog.CosmosHash)
	}
	if !strings.EqualFold(cosmosTx.To, virtualbank.ContractAddress.Hex()) {
		t.Fatalf("virtual tx target mismatch: got %s want %s", cosmosTx.To, virtualbank.ContractAddress.Hex())
	}
	if cosmosTx.Input != "0x" {
		t.Fatalf("virtual tx input must be empty, got %s", cosmosTx.Input)
	}

	receipt := fetchVirtualReceipt(t, ctx, proc.RPCURL(), cosmosLog.TransactionHash)
	if !receiptHasVirtualLog(receipt, true) {
		t.Fatalf("virtual Cosmos tx receipt did not include virtual logs with cosmos_hash: %#v", receipt)
	}

	valueTxHash := findValueTxHash(t, ctx, sourceRPC, generatedFrom, headAfter)
	valueReceipt := fetchVirtualReceipt(t, ctx, proc.RPCURL(), valueTxHash)
	if !receiptHasVirtualLog(valueReceipt, false) {
		t.Fatalf("EVM value tx receipt did not include native bank side-effect virtual logs without cosmos_hash: hash=%s receipt=%#v", valueTxHash, valueReceipt)
	}
}

type virtualizedLog struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	TransactionHash string   `json:"transactionHash"`
	Virtual         bool     `json:"virtual"`
	CosmosHash      *string  `json:"cosmos_hash"`
}

type virtualizedTx struct {
	Hash       string  `json:"hash"`
	To         string  `json:"to"`
	Input      string  `json:"input"`
	Value      string  `json:"value"`
	Virtual    bool    `json:"virtual"`
	CosmosHash *string `json:"cosmos_hash"`
}

type virtualizedReceipt struct {
	TransactionHash string           `json:"transactionHash"`
	Logs            []virtualizedLog `json:"logs"`
}

func collectVirtualLogs(t *testing.T, ctx context.Context, rpcURL string, from, to int64) []virtualizedLog {
	t.Helper()

	filter := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", from),
		"toBlock":   fmt.Sprintf("0x%x", to),
		"address":   virtualbank.ContractAddress.Hex(),
	}
	var logs []virtualizedLog
	if err := rpcCall(ctx, rpcURL, "eth_getLogs", []interface{}{filter}, &logs); err != nil {
		t.Fatalf("eth_getLogs virtual filter failed: %v", err)
	}
	return logs
}

func fetchVirtualTx(t *testing.T, ctx context.Context, rpcURL, hash string) virtualizedTx {
	t.Helper()

	var tx virtualizedTx
	if err := rpcCall(ctx, rpcURL, "eth_getTransactionByHash", []interface{}{hash}, &tx); err != nil {
		t.Fatalf("eth_getTransactionByHash %s failed: %v", hash, err)
	}
	return tx
}

func fetchVirtualReceipt(t *testing.T, ctx context.Context, rpcURL, hash string) virtualizedReceipt {
	t.Helper()

	var receipt virtualizedReceipt
	if err := rpcCall(ctx, rpcURL, "eth_getTransactionReceipt", []interface{}{hash}, &receipt); err != nil {
		t.Fatalf("eth_getTransactionReceipt %s failed: %v", hash, err)
	}
	return receipt
}

func receiptHasVirtualLog(receipt virtualizedReceipt, requireCosmosHash bool) bool {
	for _, log := range receipt.Logs {
		if !log.Virtual || !strings.EqualFold(log.Address, virtualbank.ContractAddress.Hex()) {
			continue
		}
		if requireCosmosHash && log.CosmosHash == nil {
			continue
		}
		if !requireCosmosHash && log.CosmosHash != nil {
			continue
		}
		return true
	}
	return false
}

func findValueTxHash(t *testing.T, ctx context.Context, sourceRPC string, from, to int64) string {
	t.Helper()

	candidates, err := discoverTxCandidatesInRange(ctx, sourceRPC, from, to, 256)
	if err != nil {
		t.Fatalf("discover tx candidates: %v", err)
	}
	for _, candidate := range candidates {
		var tx virtualizedTx
		if err := rpcCall(ctx, sourceRPC, "eth_getTransactionByHash", []interface{}{candidate.Hash}, &tx); err != nil {
			continue
		}
		if tx.Value != "" && tx.Value != "0x0" {
			return candidate.Hash
		}
	}
	t.Fatalf("no EVM value transaction discovered in range [%d,%d]", from, to)
	return ""
}
