package types

import (
	"fmt"
	"math/big"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/ethereum/go-ethereum/common"
	protov2 "google.golang.org/protobuf/proto"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

type mockTx struct {
	msgs []sdk.Msg
}

func (m mockTx) GetMsgs() []sdk.Msg {
	return m.msgs
}

func (mockTx) GetMsgsV2() ([]protov2.Message, error) {
	return nil, nil
}

func TestParseTxResultFallsBackToPlainTextEVMError(t *testing.T) {
	ethMsg := evmtypes.NewTxContract(
		big.NewInt(1),
		7,
		big.NewInt(0),
		4298217,
		big.NewInt(320000000),
		nil,
		nil,
		nil,
		nil,
	)

	result := &abci.ExecTxResult{
		Code:      24,
		Codespace: evmtypes.ModuleName,
		GasUsed:   0,
		Log:       "failed to execute message; message index: 0: failed to apply transaction: failed to apply ethereum core message: failed to create new contract: EVM Create operation is not authorized for user",
	}

	parsed, err := ParseTxResult(result, mockTx{msgs: []sdk.Msg{ethMsg}})
	if err != nil {
		t.Fatalf("ParseTxResult returned error: %v", err)
	}

	got := parsed.GetTxByMsgIndex(0)
	if got == nil {
		t.Fatal("expected parsed tx at msg index 0")
	}
	if !got.Failed {
		t.Fatal("expected transaction to be marked as failed")
	}
	if got.Hash != ethMsg.Hash() {
		t.Fatalf("unexpected tx hash: got %s want %s", got.Hash.Hex(), ethMsg.Hash().Hex())
	}
	if got.GasUsed != ethMsg.GetGas() {
		t.Fatalf("unexpected gas used: got %d want %d", got.GasUsed, ethMsg.GetGas())
	}
	if got.VMError != "failed to apply transaction: failed to apply ethereum core message: failed to create new contract: EVM Create operation is not authorized for user" {
		t.Fatalf("unexpected vm error: %q", got.VMError)
	}
	if got.Reason != "" {
		t.Fatalf("unexpected reason: %q", got.Reason)
	}
}

func TestParseTxResultKeepsStructuredVMErrorFields(t *testing.T) {
	ethMsg := evmtypes.NewTx(
		big.NewInt(1),
		3,
		ptrToAddress(common.HexToAddress("0x1000000000000000000000000000000000000001")),
		big.NewInt(0),
		21000,
		big.NewInt(1),
		nil,
		nil,
		nil,
		nil,
	)

	hash := ethMsg.Hash().Hex()
	result := &abci.ExecTxResult{
		Code:      24,
		Codespace: evmtypes.ModuleName,
		GasUsed:   12345,
		Log: fmt.Sprintf(
			`failed to execute message; message index: 0: {"tx_hash":"%s","gas_used":12345,"reason":"denied","vm_error":"execution reverted"}`,
			hash,
		),
	}

	parsed, err := ParseTxResult(result, mockTx{msgs: []sdk.Msg{ethMsg}})
	if err != nil {
		t.Fatalf("ParseTxResult returned error: %v", err)
	}

	got := parsed.GetTxByMsgIndex(0)
	if got == nil {
		t.Fatal("expected parsed tx at msg index 0")
	}
	if got.Hash.Hex() != hash {
		t.Fatalf("unexpected tx hash: got %s want %s", got.Hash.Hex(), hash)
	}
	if got.GasUsed != 12345 {
		t.Fatalf("unexpected gas used: got %d want %d", got.GasUsed, uint64(12345))
	}
	if got.Reason != "denied" {
		t.Fatalf("unexpected reason: %q", got.Reason)
	}
	if got.VMError != "execution reverted" {
		t.Fatalf("unexpected vm error: %q", got.VMError)
	}
}

func TestParseTxResultParsesEventsAndHelpers(t *testing.T) {
	hashA := common.HexToHash("0x1000000000000000000000000000000000000000000000000000000000000001")
	hashB := common.HexToHash("0x2000000000000000000000000000000000000000000000000000000000000002")
	result := &abci.ExecTxResult{
		Events: []abci.Event{
			{
				Type: evmtypes.EventTypeEthereumTx,
				Attributes: []abci.EventAttribute{
					{Key: evmtypes.AttributeKeyEthereumTxHash, Value: hashA.Hex()},
					{Key: evmtypes.AttributeKeyTxIndex, Value: "7"},
					{Key: evmtypes.AttributeKeyTxGasUsed, Value: "11"},
				},
			},
			{
				Type: evmtypes.EventTypeEthereumTx,
				Attributes: []abci.EventAttribute{
					{Key: evmtypes.AttributeKeyEthereumTxHash, Value: hashB.Hex()},
					{Key: evmtypes.AttributeKeyTxIndex, Value: "8"},
					{Key: evmtypes.AttributeKeyTxGasUsed, Value: "22"},
					{Key: evmtypes.AttributeKeyEthereumTxFailed, Value: "true"},
				},
			},
		},
	}

	parsed, err := ParseTxResult(result, nil)
	if err != nil {
		t.Fatalf("ParseTxResult returned error: %v", err)
	}
	if got := parsed.GetTxByHash(hashA); got == nil || got.GasUsed != 11 {
		t.Fatalf("unexpected tx by hash: %#v", got)
	}
	if got := parsed.GetTxByHash(common.HexToHash("0xffff")); got != nil {
		t.Fatalf("expected missing hash lookup to return nil, got %#v", got)
	}
	if got := parsed.GetTxByMsgIndex(-1); got != nil {
		t.Fatalf("expected negative msg index to return nil, got %#v", got)
	}
	if got := parsed.GetTxByTxIndex(8); got == nil || got.Hash != hashB || !got.Failed {
		t.Fatalf("unexpected tx by tx index: %#v", got)
	}
	if got := parsed.GetTxByTxIndex(3); got != nil {
		t.Fatalf("expected missing tx index to return nil, got %#v", got)
	}
	if got := parsed.AccumulativeGasUsed(1); got != 33 {
		t.Fatalf("unexpected accumulative gas used: %d", got)
	}
}

func TestParseTxResultNonEVMFailureUsesGasLimit(t *testing.T) {
	ethMsg := evmtypes.NewTx(
		big.NewInt(1),
		1,
		ptrToAddress(common.HexToAddress("0x3000000000000000000000000000000000000003")),
		big.NewInt(0),
		50000,
		big.NewInt(1),
		nil,
		nil,
		nil,
		nil,
	)
	result := &abci.ExecTxResult{
		Code:      5,
		Codespace: "sdk",
		Events: []abci.Event{
			{
				Type: evmtypes.EventTypeEthereumTx,
				Attributes: []abci.EventAttribute{
					{Key: evmtypes.AttributeKeyEthereumTxHash, Value: ethMsg.Hash().Hex()},
					{Key: evmtypes.AttributeKeyTxIndex, Value: "0"},
					{Key: evmtypes.AttributeKeyTxGasUsed, Value: "7"},
				},
			},
		},
	}

	parsed, err := ParseTxResult(result, mockTx{msgs: []sdk.Msg{ethMsg}})
	if err != nil {
		t.Fatalf("ParseTxResult returned error: %v", err)
	}
	got := parsed.GetTxByMsgIndex(0)
	if got == nil || !got.Failed || got.GasUsed != ethMsg.GetGas() {
		t.Fatalf("unexpected non-evm failure tx: %#v", got)
	}
}

func TestParseTxIndexerResultAndFillHelpers(t *testing.T) {
	hash := common.HexToHash("0x4000000000000000000000000000000000000000000000000000000000000004")
	resultTx := &cmrpctypes.ResultTx{
		Height: 12,
		Index:  3,
		TxResult: abci.ExecTxResult{
			Events: []abci.Event{
				{
					Type: evmtypes.EventTypeEthereumTx,
					Attributes: []abci.EventAttribute{
						{Key: evmtypes.AttributeKeyEthereumTxHash, Value: hash.Hex()},
						{Key: evmtypes.AttributeKeyTxIndex, Value: "2"},
						{Key: evmtypes.AttributeKeyTxGasUsed, Value: "99"},
					},
				},
			},
		},
	}

	txResult, err := ParseTxIndexerResult(resultTx, nil, func(txs *ParsedTxs) *ParsedTx {
		return txs.GetTxByHash(hash)
	})
	if err != nil {
		t.Fatalf("ParseTxIndexerResult returned error: %v", err)
	}
	if txResult.Height != 12 || txResult.TxIndex != 3 || txResult.EthTxIndex != 2 || txResult.GasUsed != 99 {
		t.Fatalf("unexpected tx indexer result: %#v", txResult)
	}

	if _, err := ParseTxIndexerResult(resultTx, nil, func(*ParsedTxs) *ParsedTx { return nil }); err == nil {
		t.Fatal("expected missing getter result error")
	}

	tx := NewParsedTx(4)
	if err := fillTxAttribute(&tx, []byte("ignored"), []byte("value")); err != nil {
		t.Fatalf("unexpected error for ignored attribute: %v", err)
	}
	if err := fillTxAttribute(&tx, []byte(evmtypes.AttributeKeyTxIndex), []byte("bad")); err == nil {
		t.Fatal("expected tx index parse error")
	}
	if err := fillTxAttributes(&tx, []abci.EventAttribute{{Key: evmtypes.AttributeKeyTxGasUsed, Value: "bad"}}); err == nil {
		t.Fatal("expected gas used parse error")
	}
	if err := fillTxAttributes(&tx, []abci.EventAttribute{
		{Key: evmtypes.AttributeKeyEthereumTxHash, Value: hash.Hex()},
		{Key: evmtypes.AttributeKeyTxIndex, Value: "5"},
		{Key: evmtypes.AttributeKeyTxGasUsed, Value: "101"},
		{Key: evmtypes.AttributeKeyEthereumTxFailed, Value: "x"},
	}); err != nil {
		t.Fatalf("unexpected error filling tx attributes: %v", err)
	}
	if tx.Hash != hash || tx.EthTxIndex != 5 || tx.GasUsed != 101 || !tx.Failed {
		t.Fatalf("unexpected filled tx: %#v", tx)
	}
}

func TestParseFromLogAndFailedFallbackErrors(t *testing.T) {
	parsed := &ParsedTxs{TxHashes: make(map[common.Hash]int)}
	if err := parsed.parseFromLog(&abci.ExecTxResult{Log: "plain log"}, nil); err == nil {
		t.Fatal("expected parseFromLog to reject non-matching log text")
	}

	result := &abci.ExecTxResult{
		Log: "failed to execute message; message index: 1: not-json",
	}
	if err := parsed.parseFromLog(result, mockTx{msgs: []sdk.Msg{&txtypes.Tx{}}}); err == nil {
		t.Fatal("expected parseFromLog fallback to fail for out-of-bounds msg index")
	}

	if _, err := failedTxFromTextLog(nil, result, 0, "vm error"); err == nil {
		t.Fatal("expected nil tx fallback error")
	}
	if _, err := failedTxFromTextLog(mockTx{msgs: []sdk.Msg{&txtypes.Tx{}}}, result, 0, "vm error"); err == nil {
		t.Fatal("expected non-ethereum message fallback error")
	}
}

func ptrToAddress(addr common.Address) *common.Address {
	return &addr
}
