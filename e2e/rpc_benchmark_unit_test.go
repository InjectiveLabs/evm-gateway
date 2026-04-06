package e2e

import (
	"reflect"
	"testing"
)

func TestBuildBenchmarkBatchesUsesOfflineSafeMixedRPCSet(t *testing.T) {
	fixtures := benchmarkFixtures{
		HeaderBlocks: []benchmarkBlockLookup{{
			NumberTag: "0x10",
			Hash:      "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		FullBlocks: []benchmarkBlockLookup{{
			NumberTag: "0x11",
			Hash:      "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
		RangeFilters: []map[string]interface{}{{
			"fromBlock": "0x10",
			"toBlock":   "0x11",
		}},
		TxLookups: []benchmarkTxLookup{{
			Hash:             "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			BlockHash:        "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			BlockNumber:      "0x11",
			TransactionIndex: "0x0",
		}},
	}

	batches := buildBenchmarkBatches(fixtures)
	if len(batches) != 1 {
		t.Fatalf("unexpected batch count: got %d want 1", len(batches))
	}

	methods := make([]string, 0, len(batches[0]))
	for _, req := range batches[0] {
		methods = append(methods, req.Method)
	}

	want := []string{
		"eth_chainId",
		"eth_getBlockByNumber",
		"eth_getBlockByHash",
		"eth_getBlockByNumber",
		"eth_getBlockByHash",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getLogs",
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("unexpected batch methods: got %v want %v", methods, want)
	}
}

func TestBuildBenchmarkScenariosStrictCacheOnlyIncludesOfflineBlockAndBatchMix(t *testing.T) {
	cfg := benchmarkConfig{
		StrictCacheOnly: true,
		WorkerScale:     1,
	}
	fixtures := benchmarkFixtures{
		HeaderBlocks: []benchmarkBlockLookup{{
			NumberTag: "0x10",
			Hash:      "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		FullBlocks: []benchmarkBlockLookup{{
			NumberTag: "0x11",
			Hash:      "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
		RangeFilters: []map[string]interface{}{{
			"fromBlock": "0x10",
			"toBlock":   "0x11",
		}},
		TxLookups: []benchmarkTxLookup{{
			Hash:             "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			BlockHash:        "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			BlockNumber:      "0x11",
			TransactionIndex: "0x0",
		}},
		BatchRequests: [][]rpcEnvelope{{
			{JSONRPC: "2.0", ID: 1, Method: "eth_getBlockByNumber", Params: []interface{}{"0x10", false}},
		}},
		TraceHashes: []string{"0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
	}

	scenarios := buildBenchmarkScenarios(cfg, fixtures)
	names := make([]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		names = append(names, scenario.Name)
	}

	want := []string{
		"eth_getBlockByNumber_false",
		"eth_getBlockByNumber_true",
		"eth_getBlockByHash_false",
		"eth_getBlockByHash_true",
		"eth_getLogs",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getTransactionByBlockHashAndIndex",
		"batch_mixed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("unexpected strict-cache scenarios: got %v want %v", names, want)
	}
}
