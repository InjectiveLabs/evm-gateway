package backend

import (
	"fmt"
	"testing"

	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func receiptUint(t *testing.T, receipt map[string]interface{}, key string) uint64 {
	t.Helper()
	switch v := receipt[key].(type) {
	case hexutil.Uint64:
		return uint64(v)
	case hexutil.Uint:
		return uint64(v)
	case uint64:
		return v
	default:
		t.Fatalf("unexpected %s type %T", key, receipt[key])
		return 0
	}
}

func receiptHash(t *testing.T, receipt map[string]interface{}) common.Hash {
	t.Helper()
	switch v := receipt["transactionHash"].(type) {
	case common.Hash:
		return v
	case string:
		return common.HexToHash(v)
	default:
		t.Fatalf("unexpected transactionHash type %T", v)
		return common.Hash{}
	}
}

// TestLiveBlockReceiptsSkipAnteFailedTxs builds receipts from live block
// results (no indexer): ante-failed txs are not exposed and failures after the
// ante handler report the chain gas used.
func TestLiveBlockReceiptsSkipAnteFailedTxs(t *testing.T) {
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet})

	for _, height := range mainnetfx.Heights {
		receipts, err := b.liveBlockReceipts(comet.blocks[height])
		if err != nil {
			t.Fatalf("liveBlockReceipts(%d): %v", height, err)
		}
		visible := mainnetfx.VisibleTxs[height]
		got := make([]common.Hash, 0, len(receipts))
		for i, receipt := range receipts {
			got = append(got, receiptHash(t, receipt))
			if idx := receiptUint(t, receipt, "transactionIndex"); idx != uint64(i) {
				t.Fatalf("height %d receipt %d: transactionIndex %d", height, i, idx)
			}
		}
		assertHashList(t, fmt.Sprintf("height %d receipts", height), got, visible)
	}

	receipts, err := b.liveBlockReceipts(comet.blocks[mainnetfx.HeightEx2Included])
	if err != nil {
		t.Fatalf("liveBlockReceipts: %v", err)
	}
	for i := 0; i < 4; i++ {
		if status := receiptUint(t, receipts[i], "status"); status != 0 {
			t.Fatalf("receipt %d: status %d", i, status)
		}
		if gas := receiptUint(t, receipts[i], "gasUsed"); gas != mainnetfx.GasLimitAnteFailed {
			t.Fatalf("receipt %d: gasUsed %d want %d", i, gas, mainnetfx.GasLimitAnteFailed)
		}
	}
	if status := receiptUint(t, receipts[4], "status"); status != 1 {
		t.Fatalf("receipt 4: status %d", status)
	}
}

// TestLiveVirtualBankViewSkipsAnteFailedTxs covers the virtualized live view.
func TestLiveVirtualBankViewSkipsAnteFailedTxs(t *testing.T) {
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet, virtual: true})

	for _, height := range mainnetfx.Heights {
		view, err := b.liveVirtualBankBlockView(comet.blocks[height], comet.results[height])
		if err != nil {
			t.Fatalf("liveVirtualBankBlockView(%d): %v", height, err)
		}
		hidden := make(map[common.Hash]bool)
		for _, tx := range mainnetfx.AnteFailedTxs[height] {
			hidden[tx.Hash] = true
		}
		visible := make(map[common.Hash]bool)
		for _, hash := range mainnetfx.VisibleTxs[height] {
			visible[hash] = true
		}

		seen := 0
		for i, receipt := range view.Receipts {
			hash := receiptHash(t, receipt)
			if hidden[hash] && !visible[hash] {
				t.Fatalf("height %d: ante-failed tx %s exposed", height, hash.Hex())
			}
			if idx := receiptUint(t, receipt, "transactionIndex"); idx != uint64(i) {
				t.Fatalf("height %d receipt %d: transactionIndex %d", height, i, idx)
			}
			if !visible[hash] {
				continue
			}
			seen++
			if height == mainnetfx.HeightEx2Included && receiptUint(t, receipt, "status") == 0 {
				if gas := receiptUint(t, receipt, "gasUsed"); gas != mainnetfx.GasLimitAnteFailed {
					t.Fatalf("failed receipt %s: gasUsed %d", hash.Hex(), gas)
				}
			}
		}
		if seen != len(visible) {
			t.Fatalf("height %d: %d ethereum receipts want %d", height, seen, len(visible))
		}
		if len(view.Transactions) != len(view.Receipts) {
			t.Fatalf("height %d: %d txs but %d receipts", height, len(view.Transactions), len(view.Receipts))
		}
	}
}

func TestEthMsgsFromTendermintBlockFiltersAnteFailedTxs(t *testing.T) {
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet})
	height := mainnetfx.HeightEx2Failed

	assertHashList(t, "filtered", msgHashes(b.EthMsgsFromTendermintBlock(comet.blocks[height])), mainnetfx.VisibleTxs[height])
	if count := b.GetBlockTransactionCount(comet.blocks[height]); count == nil || int(*count) != len(mainnetfx.VisibleTxs[height]) {
		t.Fatalf("GetBlockTransactionCount: %v", count)
	}
	if b.EthMsgsFromTendermintBlock(nil) != nil {
		t.Fatalf("nil block must yield no msgs")
	}

	// without block results nothing can be filtered
	comet.resultsErr = errFixtureUnavailable
	all := msgHashes(fixtureEthMsgs(t, height))
	assertHashList(t, "unfiltered", msgHashes(b.EthMsgsFromTendermintBlock(comet.blocks[height])), all)
	comet.resultsErr = nil
	delete(comet.results, height)
	assertHashList(t, "nil results", msgHashes(b.EthMsgsFromTendermintBlock(comet.blocks[height])), all)
}

func TestEthMsgsFromBlockLimits(t *testing.T) {
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet})
	height := mainnetfx.HeightEx1Failed
	block := comet.blocks[height].Block
	results := comet.results[height].TxResults

	if b.ethMsgsFromBlock(nil, results, 10) != nil {
		t.Fatalf("nil block must yield no msgs")
	}
	assertHashList(t, "clamped", msgHashes(b.ethMsgsFromBlock(block, results, len(block.Txs)+10)), mainnetfx.VisibleTxs[height])
	// cosmos txs 0..10: only the first ethereum tx precedes the ante-failed ones
	assertHashList(t, "prefix", msgHashes(b.ethMsgsFromBlock(block, results, 11)), mainnetfx.VisibleTxs[height][:1])
	// fewer results than txs: the uncovered txs are not filtered
	all := msgHashes(fixtureEthMsgs(t, height))
	assertHashList(t, "short results", msgHashes(b.ethMsgsFromBlock(block, results[:11], len(block.Txs))), all)

	undecodable := &tmtypes.Block{Header: block.Header, Data: tmtypes.Data{Txs: tmtypes.Txs{[]byte("garbage")}}}
	if msgs := b.ethMsgsFromBlock(undecodable, nil, 1); len(msgs) != 0 {
		t.Fatalf("undecodable tx must be skipped: %v", msgs)
	}
}

// TestGetTransactionByBlockAndIndexLiveSkipsAnteFailedTxs resolves txs by
// index from live data when neither the indexer nor the Comet tx index can.
func TestGetTransactionByBlockAndIndexLiveSkipsAnteFailedTxs(t *testing.T) {
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet})
	height := mainnetfx.HeightEx2Failed

	for i, want := range mainnetfx.VisibleTxs[height] {
		tx, err := b.GetTransactionByBlockAndIndex(comet.blocks[height], hexutil.Uint(i))
		if err != nil {
			t.Fatalf("GetTransactionByBlockAndIndex(%d): %v", i, err)
		}
		if tx == nil || tx.Hash != want {
			t.Fatalf("index %d: got %v want %s", i, tx, want.Hex())
		}
	}
	tx, err := b.GetTransactionByBlockAndIndex(comet.blocks[height], hexutil.Uint(len(mainnetfx.VisibleTxs[height])))
	if err != nil || tx != nil {
		t.Fatalf("out of range index: %v %v", tx, err)
	}
}
