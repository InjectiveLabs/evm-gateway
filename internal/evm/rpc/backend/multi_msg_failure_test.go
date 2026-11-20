package backend

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

// Mainnet block 185519530, Cosmos tx #5: message 0 reverted, so the Cosmos tx
// failed (code 3, evm codespace) and message 1 never executed. Both consumed
// their nonce. The indexer used to reject the block ("msg index not found in
// results") and skip it, so eth_getBlockReceipts (live path) returned both
// txs while eth_getTransactionByHash and eth_getTransactionReceipt returned
// null.
func TestMultiMsgFailureLookupsMatchBlockReceipts(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	want := []struct {
		hash    common.Hash
		index   uint64 // without virtualization
		gasUsed uint64
	}{
		{mainnetfx.TxMultiMsgReverted, 1, mainnetfx.GasUsedMultiMsgReverted},
		{mainnetfx.TxMultiMsgUnexecuted, 2, mainnetfx.GasLimitMultiMsgUnexecuted},
	}

	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprintf("virtual=%v", virtual), func(t *testing.T) {
			comet := newFixtureComet(t)
			kv, _ := newFixtureKV(t, virtual)
			if err := kv.IndexBlockWithResults(comet.blocks[height].Block, comet.results[height]); err != nil {
				t.Fatalf("index block %d: %v", height, err)
			}
			cached := newFixtureBackend(t, fixtureBackendOptions{virtual: virtual, offline: true, indexer: kv})
			live := newFixtureBackend(t, fixtureBackendOptions{virtual: virtual, comet: comet})

			blockNr := rpctypes.BlockNumber(height)
			blockReceipts, err := cached.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNr})
			if err != nil {
				t.Fatalf("GetBlockReceipts: %v", err)
			}
			liveReceipts, err := live.liveBlockReceipts(comet.blocks[height])
			if err != nil {
				t.Fatalf("liveBlockReceipts: %v", err)
			}
			// The live path omits reason/vmError and serializes geth's log
			// blockTimestamp; every other field must match.
			if got, want := comparableReceiptsJSON(t, blockReceipts), comparableReceiptsJSON(t, liveReceipts); got != want {
				t.Fatalf("indexed block receipts differ from live ones:\nindexed %s\nlive    %s", got, want)
			}

			positions := make(map[common.Hash]uint64, len(blockReceipts))
			for i, receipt := range blockReceipts {
				positions[receiptHash(t, receipt)] = uint64(i)
			}
			for _, w := range want {
				index, ok := positions[w.hash]
				if !ok {
					t.Fatalf("block receipts miss %s", w.hash.Hex())
				}
				if !virtual && index != w.index {
					t.Fatalf("block receipt %s at index %d want %d", w.hash.Hex(), index, w.index)
				}
				w.index = index

				tx, err := cached.GetTransactionByHash(w.hash)
				if err != nil || tx == nil {
					t.Fatalf("GetTransactionByHash(%s) = %v, %v", w.hash.Hex(), tx, err)
				}
				if tx.BlockNumber.ToInt().Int64() != height || uint64(*tx.TransactionIndex) != w.index {
					t.Fatalf("tx %s at block %v index %v", w.hash.Hex(), tx.BlockNumber, *tx.TransactionIndex)
				}

				receipt, err := cached.GetTransactionReceipt(w.hash)
				if err != nil || receipt == nil {
					t.Fatalf("GetTransactionReceipt(%s) = %v, %v", w.hash.Hex(), receipt, err)
				}
				if status := receiptUint(t, receipt, "status"); status != 0 {
					t.Fatalf("receipt %s: status %d", w.hash.Hex(), status)
				}
				if gas := receiptUint(t, receipt, "gasUsed"); gas != w.gasUsed {
					t.Fatalf("receipt %s: gasUsed %d want %d", w.hash.Hex(), gas, w.gasUsed)
				}
				if got, want := mustJSON(t, receipt), mustJSON(t, blockReceipts[w.index]); got != want {
					t.Fatalf("receipt %s differs from its block receipt:\nby hash %s\nblock   %s", w.hash.Hex(), got, want)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	bz, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(bz)
}

func comparableReceiptsJSON(t *testing.T, receipts []map[string]interface{}) string {
	t.Helper()
	var decoded []map[string]interface{}
	if err := json.Unmarshal([]byte(mustJSON(t, receipts)), &decoded); err != nil {
		t.Fatalf("unmarshal receipts: %v", err)
	}
	for _, receipt := range decoded {
		delete(receipt, "reason")
		delete(receipt, "vmError")
		logs, _ := receipt["logs"].([]interface{})
		for _, log := range logs {
			if m, ok := log.(map[string]interface{}); ok {
				delete(m, "blockTimestamp")
			}
		}
	}
	return mustJSON(t, decoded)
}
