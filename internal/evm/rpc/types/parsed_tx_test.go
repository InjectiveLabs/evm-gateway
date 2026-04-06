package types

import (
	"fmt"
	"math/big"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
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

func ptrToAddress(addr common.Address) *common.Address {
	return &addr
}
