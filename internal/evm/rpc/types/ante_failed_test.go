package types

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestTxAnteFailed(t *testing.T) {
	testCases := []struct {
		name string
		res  *abci.ExecTxResult
		want bool
	}{
		{name: "nil result", res: nil, want: false},
		{name: "success", res: &abci.ExecTxResult{Code: abci.CodeTypeOK}, want: false},
		{
			name: "evm codespace failure consumed the nonce",
			res:  &abci.ExecTxResult{Code: 3, Codespace: evmtypes.ModuleName, Log: "execution reverted"},
			want: false,
		},
		{
			name: "legacy block gas limit failure consumed the nonce",
			res:  &abci.ExecTxResult{Code: 11, Codespace: "sdk", Log: ExceedBlockGasLimitError + " 1200000"},
			want: false,
		},
		{
			name: "message execution failure after the ante handler",
			res: &abci.ExecTxResult{
				Code:      1,
				Codespace: "undefined",
				Log:       `failed to execute message; message index: 0: {"tx_hash":"0x43","gas_used":28209}`,
			},
			want: false,
		},
		{
			name: "ante insufficient funds",
			res: &abci.ExecTxResult{
				Code:      5,
				Codespace: "sdk",
				Log:       "failed to transfer 500000000000000000 from address 0x10 using the EVM block context transfer function: insufficient funds",
			},
			want: true,
		},
		{
			name: "ante invalid nonce",
			res:  &abci.ExecTxResult{Code: 32, Codespace: "sdk", Log: "invalid nonce; got 5, expected 6: invalid sequence"},
			want: true,
		},
		// Shapes observed on mainnet: panics recovered during message execution
		// carry no "failed to execute message" wrapper but keep the ante events
		// (fee deduction), i.e. the nonce was consumed.
		{
			name: "panic recovered during message execution (mainnet 151947794 tx 17)",
			res: &abci.ExecTxResult{
				Code:      111222,
				Codespace: "undefined",
				Log:       "recovered: runtime error: invalid memory address or nil pointer dereference\nstack:\n...",
				Events:    anteEvents(),
			},
			want: false,
		},
		{
			name: "out of gas panic during message execution (mainnet 182270453 tx 58)",
			res: &abci.ExecTxResult{
				Code:      11,
				Codespace: "sdk",
				Log:       "out of gas in location: IterNextFlat; gasWanted: 47922, gasUsed: 0: out of gas",
				Events:    anteEvents(),
			},
			want: false,
		},
		{
			name: "same out of gas log without ante events failed in the ante handler",
			res: &abci.ExecTxResult{
				Code:      11,
				Codespace: "sdk",
				Log:       "out of gas in location: IterNextFlat; gasWanted: 47922, gasUsed: 0: out of gas",
			},
			want: true,
		},
		{
			name: "ante out of gas without message wrapper",
			res:  &abci.ExecTxResult{Code: 11, Codespace: "sdk", Log: "tx gas (99) exceeds block gas limit (10): out of gas"},
			want: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TxAnteFailed(tc.res); got != tc.want {
				t.Fatalf("TxAnteFailed: got %v want %v", got, tc.want)
			}
		})
	}
}

func TestTxAnteFailedMatchesMainnetFixtures(t *testing.T) {
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	decoder := clientCtx.TxConfig.TxDecoder()

	for _, height := range mainnetfx.Heights {
		block, err := mainnetfx.Block(height)
		if err != nil {
			t.Fatalf("Block(%d): %v", height, err)
		}
		results, err := mainnetfx.BlockResults(height)
		if err != nil {
			t.Fatalf("BlockResults(%d): %v", height, err)
		}

		var got []mainnetfx.AnteFailedTx
		for txIndex, txBz := range block.Txs {
			if !TxAnteFailed(results.TxResults[txIndex]) {
				continue
			}
			tx, err := decoder(txBz)
			if err != nil {
				t.Fatalf("decode tx %d: %v", txIndex, err)
			}
			for _, msg := range tx.GetMsgs() {
				if ethMsg, ok := msg.(*evmtypes.MsgEthereumTx); ok {
					got = append(got, mainnetfx.AnteFailedTx{TxIndex: uint32(txIndex), Hash: ethMsg.Hash()})
				}
			}
		}

		want := mainnetfx.AnteFailedTxs[height]
		if len(got) != len(want) {
			t.Fatalf("height %d: ante-failed %v want %v", height, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("height %d: ante-failed[%d] %v want %v", height, i, got[i], want[i])
			}
		}
	}
}

// anteEvents mirrors the events a successful EVM ante handler leaves on a
// failed tx result: fee deduction transfers and the fee event.
func anteEvents() []abci.Event {
	return []abci.Event{
		{Type: "coin_spent"},
		{Type: "transfer"},
		{Type: "tx", Attributes: []abci.EventAttribute{{Key: "fee", Value: "1inj"}}},
	}
}
