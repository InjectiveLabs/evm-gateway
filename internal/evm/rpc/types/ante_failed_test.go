package types

import (
	"math/big"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"

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

func TestFailedEthTxGasUsed(t *testing.T) {
	msg := evmtypes.NewTx(big.NewInt(1), 1, nil, big.NewInt(0), 50000, big.NewInt(1), nil, nil, nil, nil)
	testCases := []struct {
		name     string
		res      *abci.ExecTxResult
		msgCount int
		want     uint64
	}{
		{name: "nil result keeps gas limit", res: nil, msgCount: 1, want: 50000},
		{
			name:     "block gas limit keeps gas limit",
			res:      &abci.ExecTxResult{Code: 11, Log: ExceedBlockGasLimitError + " 50000", GasUsed: 7},
			msgCount: 1,
			want:     50000,
		},
		{
			name:     "multi message keeps gas limit",
			res:      &abci.ExecTxResult{Code: 1, Log: MsgExecutionFailedLog, GasUsed: 7},
			msgCount: 2,
			want:     50000,
		},
		{
			name:     "negative gas used keeps gas limit",
			res:      &abci.ExecTxResult{Code: 1, Log: MsgExecutionFailedLog, GasUsed: -1},
			msgCount: 1,
			want:     50000,
		},
		{
			name:     "single message uses chain gas used",
			res:      &abci.ExecTxResult{Code: 1, Log: MsgExecutionFailedLog, GasUsed: 28209},
			msgCount: 1,
			want:     28209,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailedEthTxGasUsed(tc.res, msg, tc.msgCount); got != tc.want {
				t.Fatalf("FailedEthTxGasUsed: got %d want %d", got, tc.want)
			}
		})
	}
}

// TestTxAnteFailedMatchesMainnetFixtures classifies every tx result of the
// incident blocks and checks the Ethereum txs flagged as ante-failed.
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

	// the re-included txs failed after the ante handler and report chain gas
	results, err := mainnetfx.BlockResults(mainnetfx.HeightEx2Included)
	if err != nil {
		t.Fatalf("BlockResults: %v", err)
	}
	msg := evmtypes.NewTx(big.NewInt(1), 1, nil, big.NewInt(0), mainnetfx.GasLimitAnteFailed, big.NewInt(1), nil, nil, nil, nil)
	for i := 1; i <= 4; i++ {
		if got := FailedEthTxGasUsed(results.TxResults[i], msg, 1); got != mainnetfx.GasUsedEx2Included {
			t.Fatalf("tx %d: gas used %d want %d", i, got, mainnetfx.GasUsedEx2Included)
		}
	}
}

// TestParseTxResultNonEVMFailureGuards covers events without a matching
// message and non-Ethereum messages in a failed tx.
func TestParseTxResultNonEVMFailureGuards(t *testing.T) {
	ethMsg := evmtypes.NewTx(big.NewInt(1), 1, nil, big.NewInt(0), 50000, big.NewInt(1), nil, nil, nil, nil)
	event := func(hash common.Hash, index string) abci.Event {
		return abci.Event{
			Type: evmtypes.EventTypeEthereumTx,
			Attributes: []abci.EventAttribute{
				{Key: evmtypes.AttributeKeyEthereumTxHash, Value: hash.Hex()},
				{Key: evmtypes.AttributeKeyTxIndex, Value: index},
				{Key: evmtypes.AttributeKeyTxGasUsed, Value: "7"},
			},
		}
	}
	result := &abci.ExecTxResult{
		Code:      1,
		Codespace: "undefined",
		Log:       MsgExecutionFailedLog + "; message index: 0: boom",
		GasUsed:   900,
		Events: []abci.Event{
			event(common.HexToHash("0x01"), "0"),
			event(ethMsg.Hash(), "1"),
			event(common.HexToHash("0x03"), "2"),
		},
	}

	// msg 0 is not an Ethereum message, msg 2 has no matching message at all
	parsed, err := ParseTxResult(result, mockTx{msgs: []sdk.Msg{&banktypes.MsgSend{}, ethMsg}})
	if err != nil {
		t.Fatalf("ParseTxResult: %v", err)
	}
	if len(parsed.Txs) != 3 {
		t.Fatalf("unexpected parsed tx count %d", len(parsed.Txs))
	}
	for i, tx := range parsed.Txs {
		if !tx.Failed {
			t.Fatalf("tx %d not marked failed", i)
		}
	}
	if parsed.Txs[0].GasUsed != 7 || parsed.Txs[2].GasUsed != 7 {
		t.Fatalf("unmatched msgs must keep event gas: %d %d", parsed.Txs[0].GasUsed, parsed.Txs[2].GasUsed)
	}
	// two messages in the tx: the cosmos gas can't be attributed
	if parsed.Txs[1].GasUsed != ethMsg.GetGas() {
		t.Fatalf("multi message failure gas: got %d want %d", parsed.Txs[1].GasUsed, ethMsg.GetGas())
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
