package types

import (
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// multiMsgFailureTxIndex is Cosmos tx #5 of mainnet block 185519530.
const multiMsgFailureTxIndex = 5

func multiMsgFailureFixture(t *testing.T) (sdk.TxDecoder, [][]byte, []*abci.ExecTxResult, sdk.Tx) {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	block, err := mainnetfx.Block(mainnetfx.HeightMultiMsgFailure)
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	results, err := mainnetfx.BlockResults(mainnetfx.HeightMultiMsgFailure)
	if err != nil {
		t.Fatalf("BlockResults: %v", err)
	}
	decoder := clientCtx.TxConfig.TxDecoder()
	tx, err := decoder(block.Txs[multiMsgFailureTxIndex])
	if err != nil {
		t.Fatalf("decode tx: %v", err)
	}
	return decoder, block.Txs.ToSliceOfBytes(), results.TxResults, tx
}

func TestParseTxResultMultiMsgFailure(t *testing.T) {
	_, _, results, tx := multiMsgFailureFixture(t)
	res := results[multiMsgFailureTxIndex]
	if res.Code != 3 || res.Codespace != evmtypes.ModuleName || TxAnteFailed(res) {
		t.Fatalf("unexpected fixture result: code %d codespace %q", res.Code, res.Codespace)
	}

	parsed, err := ParseTxResult(res, tx)
	if err != nil {
		t.Fatalf("ParseTxResult: %v", err)
	}
	if len(parsed.Txs) != 2 {
		t.Fatalf("parsed %d txs, want 2", len(parsed.Txs))
	}
	reverted := parsed.GetTxByMsgIndex(0)
	if reverted == nil || reverted.Hash != mainnetfx.TxMultiMsgReverted || !reverted.Failed ||
		reverted.GasUsed != mainnetfx.GasUsedMultiMsgReverted || reverted.VMError != "execution reverted" ||
		reverted.Reason != "ERC20: transfer amount exceeds allowance" {
		t.Fatalf("message 0: %+v", reverted)
	}
	unexecuted := parsed.GetTxByMsgIndex(1)
	if unexecuted == nil || unexecuted.Hash != mainnetfx.TxMultiMsgUnexecuted || !unexecuted.Failed ||
		unexecuted.GasUsed != mainnetfx.GasLimitMultiMsgUnexecuted || unexecuted.VMError != "" || unexecuted.MsgIndex != 1 {
		t.Fatalf("message 1: %+v", unexecuted)
	}
	if parsed.GetTxByHash(mainnetfx.TxMultiMsgUnexecuted) != unexecuted || parsed.GetTxByHash(mainnetfx.TxMultiMsgReverted) != reverted {
		t.Fatal("hash lookups do not match message lookups")
	}
}

// When a later message fails, the log names only that message. Its entry used
// to be appended at position 0, so message 0 took the failed message's hash
// and message 1 was missing.
func TestParseTxResultLaterMessageFails(t *testing.T) {
	_, _, results, tx := multiMsgFailureFixture(t)
	res := *results[multiMsgFailureTxIndex]
	res.Log = strings.Replace(res.Log, "message index: 0", "message index: 1", 1)
	res.Log = strings.Replace(res.Log, strings.TrimPrefix(mainnetfx.TxMultiMsgReverted.Hex(), "0x"), strings.TrimPrefix(mainnetfx.TxMultiMsgUnexecuted.Hex(), "0x"), 1)

	parsed, err := ParseTxResult(&res, tx)
	if err != nil {
		t.Fatalf("ParseTxResult: %v", err)
	}
	first, second := parsed.GetTxByMsgIndex(0), parsed.GetTxByMsgIndex(1)
	if first == nil || first.Hash != mainnetfx.TxMultiMsgReverted || !first.Failed || first.GasUsed != 120000 || first.VMError != "" {
		t.Fatalf("message 0: %+v", first)
	}
	if second == nil || second.Hash != mainnetfx.TxMultiMsgUnexecuted || !second.Failed ||
		second.GasUsed != mainnetfx.GasUsedMultiMsgReverted || second.VMError != "execution reverted" {
		t.Fatalf("message 1: %+v", second)
	}
}

func TestNormalizeBlockTxResponseIndexesCountsAllFailedMessages(t *testing.T) {
	decoder, txs, results, tx := multiMsgFailureFixture(t)
	if got := FailedEthereumTxCount(tx, results[multiMsgFailureTxIndex]); got != 2 {
		t.Fatalf("FailedEthereumTxCount = %d, want 2", got)
	}
	// The log names message 0 only: inferring from it counts one tx.
	if got := inferFailedEthereumTxCount(results[multiMsgFailureTxIndex].Codespace, results[multiMsgFailureTxIndex].Log); got != 1 {
		t.Fatalf("inferFailedEthereumTxCount = %d, want 1", got)
	}

	// Cosmos tx #6 is the next tx with EVM logs; it is Ethereum tx 3 of the
	// block (after #0's tx and the two of #5).
	normalized, err := NormalizeBlockTxResponseIndexes(decoder, txs, results)
	if err != nil {
		t.Fatalf("NormalizeBlockTxResponseIndexes: %v", err)
	}
	inferred, err := NormalizeTxResponseIndexes(results)
	if err != nil {
		t.Fatalf("NormalizeTxResponseIndexes: %v", err)
	}
	next := firstEthLogTxIndexAfter(t, normalized, multiMsgFailureTxIndex)
	if next != 3 {
		t.Fatalf("next tx logs have tx index %d, want 3", next)
	}
	if got := firstEthLogTxIndexAfter(t, inferred, multiMsgFailureTxIndex); got != 2 {
		t.Fatalf("log inference: next tx logs have tx index %d, want 2", got)
	}
	// A failed non-EVM Cosmos tx (#2, exchange code 107) consumes no slot.
	if got := FailedEthereumTxCount(mustDecode(t, decoder, txs[2]), results[2]); got != 0 {
		t.Fatalf("failed exchange tx counts %d Ethereum txs", got)
	}
}

func mustDecode(t *testing.T, decoder sdk.TxDecoder, bz []byte) sdk.Tx {
	t.Helper()
	tx, err := decoder(bz)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return tx
}

func firstEthLogTxIndexAfter(t *testing.T, results []*abci.ExecTxResult, after int) uint64 {
	t.Helper()
	for i := after + 1; i < len(results); i++ {
		if results[i] == nil || results[i].Code != abci.CodeTypeOK || len(results[i].Data) == 0 {
			continue
		}
		logs, err := evmtypes.DecodeTxLogs(results[i].Data, uint64(mainnetfx.HeightMultiMsgFailure))
		if err != nil {
			t.Fatalf("decode logs of tx %d: %v", i, err)
		}
		if len(logs) > 0 {
			return uint64(logs[0].TxIndex)
		}
	}
	t.Fatalf("no EVM logs after tx %d", after)
	return 0
}
