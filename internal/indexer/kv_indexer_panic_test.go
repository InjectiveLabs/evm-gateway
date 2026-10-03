package indexer

import (
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

// TestIndexBlockKeepsTxWhosePanicRecoveredDuringExecution indexes a mainnet
// block with a tx whose message execution panicked after the ante handler
// succeeded (code 111222, no "failed to execute message" wrapper, ante events
// present). It consumed its nonce, so it must stay visible as a failed tx.
func TestIndexBlockKeepsTxWhosePanicRecoveredDuringExecution(t *testing.T) {
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	block, err := mainnetfx.Block(mainnetfx.HeightPanicInExecution)
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	results, err := mainnetfx.BlockResults(mainnetfx.HeightPanicInExecution)
	if err != nil {
		t.Fatalf("BlockResults: %v", err)
	}
	if res := results.TxResults[17]; res.Code != 111222 || len(res.Events) == 0 {
		t.Fatalf("unexpected fixture tx result: code %d events %d", res.Code, len(res.Events))
	}

	kv := NewKVIndexer(dbm.NewMemDB(), testLogger(), clientCtx)
	stats, err := kv.IndexBlockWithStatsAndResults(block, results)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if stats.SkippedAnteFailedEthTxs != 0 {
		t.Fatalf("no tx of this block failed in the ante handler, skipped %d", stats.SkippedAnteFailedEthTxs)
	}

	txResult, err := kv.GetByTxHash(mainnetfx.TxPanicInExecution)
	if err != nil {
		t.Fatalf("panicked tx must stay indexed: %v", err)
	}
	if txResult.Height != mainnetfx.HeightPanicInExecution || txResult.TxIndex != 17 || !txResult.Failed {
		t.Fatalf("unexpected tx result: %+v", txResult)
	}
	receipt, err := kv.GetReceiptByTxHash(mainnetfx.TxPanicInExecution)
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if receipt["status"] != hexutil.Uint(0) || receipt["blockNumber"] != hexutil.Uint64(mainnetfx.HeightPanicInExecution) {
		t.Fatalf("unexpected receipt: status %v block %v", receipt["status"], receipt["blockNumber"])
	}
	// The chain reports no gas for the panicked execution.
	if receipt["gasUsed"] != hexutil.Uint64(0) {
		t.Fatalf("unexpected gas used: %v", receipt["gasUsed"])
	}
}
