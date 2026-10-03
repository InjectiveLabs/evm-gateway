package mainnetfx

import (
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// TestFixtureConstantsMatchChainData recomputes the visible and ante-failed
// Ethereum txs of every fixture block from the raw block and block results.
func TestFixtureConstantsMatchChainData(t *testing.T) {
	clientCtx, err := ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	decoder := clientCtx.TxConfig.TxDecoder()

	for _, height := range Heights {
		block, err := Block(height)
		if err != nil {
			t.Fatalf("Block(%d): %v", height, err)
		}
		results, err := BlockResults(height)
		if err != nil {
			t.Fatalf("BlockResults(%d): %v", height, err)
		}
		if block.Height != height || results.Height != height {
			t.Fatalf("fixture height mismatch: block %d results %d want %d", block.Height, results.Height, height)
		}
		if len(block.Txs) != len(results.TxResults) {
			t.Fatalf("height %d: %d txs but %d results", height, len(block.Txs), len(results.TxResults))
		}

		var visible []common.Hash
		var anteFailed []AnteFailedTx
		for txIndex, txBz := range block.Txs {
			tx, err := decoder(txBz)
			if err != nil {
				t.Fatalf("height %d tx %d: decode: %v", height, txIndex, err)
			}
			for _, msg := range tx.GetMsgs() {
				ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
				if !ok {
					continue
				}
				if rpctypes.TxAnteFailed(results.TxResults[txIndex]) {
					anteFailed = append(anteFailed, AnteFailedTx{TxIndex: uint32(txIndex), Hash: ethMsg.Hash()})
					if ethMsg.GetGas() != GasLimitAnteFailed {
						t.Fatalf("height %d tx %d: gas limit %d want %d", height, txIndex, ethMsg.GetGas(), GasLimitAnteFailed)
					}
					continue
				}
				visible = append(visible, ethMsg.Hash())
			}
		}

		assertHashes(t, height, "visible", visible, VisibleTxs[height])
		want := AnteFailedTxs[height]
		if len(anteFailed) != len(want) {
			t.Fatalf("height %d: ante-failed %v want %v", height, anteFailed, want)
		}
		for i := range want {
			if anteFailed[i] != want[i] {
				t.Fatalf("height %d: ante-failed[%d] %v want %v", height, i, anteFailed[i], want[i])
			}
		}
	}

	for _, txResult := range mustResults(t, HeightEx2Included).TxResults[1:5] {
		if txResult.Code != 1 || uint64(txResult.GasUsed) != GasUsedEx2Included || rpctypes.TxAnteFailed(txResult) {
			t.Fatalf("unexpected re-included tx result: code %d gas %d", txResult.Code, txResult.GasUsed)
		}
	}
}

func TestFixtureAuxiliaryData(t *testing.T) {
	for _, height := range Heights {
		raw, err := SentryTrace(height)
		if err != nil || len(raw) == 0 {
			t.Fatalf("SentryTrace(%d): %v", height, err)
		}
		if _, err := ResultBlock(height); err != nil {
			t.Fatalf("ResultBlock(%d): %v", height, err)
		}
	}
	if _, err := Block(1); err == nil {
		t.Fatalf("expected missing fixture error")
	}
	if _, err := BlockResults(1); err == nil {
		t.Fatalf("expected missing fixture error")
	}
	if _, err := SentryTrace(1); err == nil {
		t.Fatalf("expected missing fixture error")
	}
	all, err := AllBlockResults()
	if err != nil || len(all) != len(Heights) {
		t.Fatalf("AllBlockResults: %d %v", len(all), err)
	}
	kv, err := LegacyReporterState()
	if err != nil || len(kv) == 0 {
		t.Fatalf("LegacyReporterState: %d %v", len(kv), err)
	}
}

func mustResults(t *testing.T, height int64) *coretypes.ResultBlockResults {
	t.Helper()
	res, err := BlockResults(height)
	if err != nil {
		t.Fatalf("BlockResults(%d): %v", height, err)
	}
	return res
}

func assertHashes(t *testing.T, height int64, label string, got, want []common.Hash) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("height %d %s: got %d hashes want %d (%v)", height, label, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("height %d %s[%d]: got %s want %s", height, label, i, got[i].Hex(), want[i].Hex())
		}
	}
}
