package e2e

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/debank"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// TestDebankPipelineCompatibilityLive validates the complete Mode 2 wire
// contract against a separately running gateway. It is intentionally isolated
// from the heavier chain-stresser E2E suite.
func TestDebankPipelineCompatibilityLive(t *testing.T) {
	if os.Getenv("WEB3INJ_E2E_DEBANK") != "1" {
		t.Skip("set WEB3INJ_E2E_DEBANK=1 to run Pipeline trace compatibility e2e")
	}

	rpcURL := getenv("WEB3INJ_E2E_DEBANK_RPC", "http://127.0.0.1:8645")
	blockParam := getenv("WEB3INJ_E2E_DEBANK_BLOCK", "latest")
	raw, err := rpcRawResult(context.Background(), rpcURL, "trace_debankBlock", []interface{}{blockParam})
	if err != nil {
		t.Fatalf("trace_debankBlock: %v", err)
	}
	var output debank.Output
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode Pipeline output: %v", err)
	}
	if output.BlockFile == nil || output.Header == nil {
		t.Fatal("response must contain block_file and header")
	}
	byHashRaw, err := rpcRawResult(context.Background(), rpcURL, "trace_debankBlock", []interface{}{output.Header.Hash.Hex()})
	if err != nil {
		t.Fatalf("trace_debankBlock by hash: %v", err)
	}
	var byHash debank.Output
	if err := json.Unmarshal(byHashRaw, &byHash); err != nil {
		t.Fatalf("decode hash-addressed Pipeline output: %v", err)
	}
	if byHash.BlockFile == nil || byHash.BlockFile.Block.ID != output.BlockFile.Block.ID || byHash.ValidationHash != output.ValidationHash {
		t.Fatalf("block hash lookup differs from number lookup: %#v", byHash.BlockFile)
	}
	if output.BlockFile.Block.ID != output.Header.Hash.Hex() {
		t.Fatalf("block hash %s differs from header hash %s", output.BlockFile.Block.ID, output.Header.Hash.Hex())
	}
	if output.BlockFile.Block.Height == nil || output.Header.Number == nil || output.BlockFile.Block.Height.Cmp(output.Header.Number.ToInt()) != 0 {
		t.Fatalf("block/header number mismatch: %v/%v", output.BlockFile.Block.Height, output.Header.Number)
	}
	if got := output.BlockFile.Validation().ValidationHash; output.ValidationHash != got {
		t.Fatalf("validation hash = %d, recomputed %d", output.ValidationHash, got)
	}

	traces := make(map[string]debank.Trace, len(output.BlockFile.Traces)+len(output.BlockFile.ErrorTraces))
	rootByTx := make(map[string]int, len(output.BlockFile.Txs))
	for _, trace := range append(append([]debank.Trace{}, output.BlockFile.Traces...), output.BlockFile.ErrorTraces...) {
		if trace.ID == "" || trace.TxID == "" {
			t.Fatalf("trace is missing identity: %#v", trace)
		}
		if _, duplicate := traces[trace.ID]; duplicate {
			t.Fatalf("duplicate trace id %s", trace.ID)
		}
		traces[trace.ID] = trace
		if trace.ParentTraceID == "" {
			rootByTx[trace.TxID]++
			if trace.ID != pipelineHash(trace.TxID, "", "0") {
				t.Fatalf("invalid root trace id %s for tx %s", trace.ID, trace.TxID)
			}
			if trace.TraceAddress == nil || len(trace.TraceAddress) != 0 {
				t.Fatalf("root trace_address must be an empty array: %#v", trace.TraceAddress)
			}
		} else {
			if trace.ID != pipelineHash(trace.TxID, trace.ParentTraceID, strconv.FormatInt(trace.PosInParentTrace, 10)) {
				t.Fatalf("invalid child trace id %s", trace.ID)
			}
		}
	}
	for _, trace := range traces {
		if trace.ParentTraceID != "" {
			if _, ok := traces[trace.ParentTraceID]; !ok {
				t.Fatalf("trace %s references absent parent %s", trace.ID, trace.ParentTraceID)
			}
		}
	}
	for _, tx := range output.BlockFile.Txs {
		if rootByTx[tx.ID] != 1 {
			t.Fatalf("transaction %s has %d root traces", tx.ID, rootByTx[tx.ID])
		}
	}

	events := append(append([]debank.Event{}, output.BlockFile.Events...), output.BlockFile.ErrorEvents...)
	for _, event := range events {
		if _, ok := traces[event.ParentTraceID]; !ok {
			t.Fatalf("event %s references absent trace %s", event.ID, event.ParentTraceID)
		}
		if event.ID != pipelineHash(event.ParentTraceID, strconv.FormatInt(event.Position, 10)) {
			t.Fatalf("invalid event id %s", event.ID)
		}
		if event.Selector != "" && !common.IsHexAddress(event.Address) {
			t.Fatalf("event %s has invalid contract address %q", event.ID, event.Address)
		}
	}
	for _, address := range output.BlockFile.StorageContracts {
		if address != strings.ToLower(address) || !common.IsHexAddress(address) {
			t.Fatalf("invalid storage contract address %q", address)
		}
	}

	if len(output.StateDiff) > 0 {
		var stateDiff debank.BlockStorageDiff
		if err := rlp.DecodeBytes(output.StateDiff, &stateDiff); err != nil {
			t.Fatalf("decode state_diff RLP: %v", err)
		}
		if stateDiff.Hash != output.Header.StateRoot {
			t.Fatalf("state diff root %s differs from header %s", stateDiff.Hash, output.Header.StateRoot)
		}
		if output.BlockFile.Block.Height.Sign() > 0 && output.BlockFile.Block.Height.Int64() > 1 {
			parentHeight := new(big.Int).Sub(output.BlockFile.Block.Height, common.Big1)
			parentTag := "0x" + parentHeight.Text(16)
			parentRaw, err := rpcRawResult(context.Background(), rpcURL, "eth_getBlockByNumber", []interface{}{parentTag, false})
			if err != nil {
				t.Fatalf("get parent block: %v", err)
			}
			var parent struct {
				StateRoot common.Hash `json:"stateRoot"`
			}
			if err := json.Unmarshal(parentRaw, &parent); err != nil {
				t.Fatalf("decode parent block: %v", err)
			}
			if stateDiff.ParentHash != parent.StateRoot {
				t.Fatalf("state diff parent root %s differs from header %s", stateDiff.ParentHash, parent.StateRoot)
			}
		}
	}

	t.Logf("validated block %s: %d txs, %d traces, %d events, %d storage contracts", output.BlockFile.Block.Height, len(output.BlockFile.Txs), len(traces), len(events), len(output.BlockFile.StorageContracts))
}

func pipelineHash(parts ...string) string {
	h := md5.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
