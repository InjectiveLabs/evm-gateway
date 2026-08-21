package virtual

import (
	"math/big"
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	virtualbank "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/bank"
	virtualibc "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/ibc"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestSyntheticTxCoalescesBankAndIBCHookEvents(t *testing.T) {
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	emitter := common.HexToAddress("0x2222222222222222222222222222222222222222")
	embeddedTopic := common.HexToHash("0x1234")
	hook := &evmtypes.EventIBCHookCall{
		DestinationPort:    "transfer",
		DestinationChannel: "channel-9",
		Sequence:           17,
		Contract:           target.Hex(),
		Input:              []byte{0xde, 0xad, 0xbe, 0xef},
		Success:            true,
		ReturnData:         []byte{0x01},
		GasUsed:            77,
		Logs: []*evmtypes.Log{{
			Address: emitter.Hex(),
			Topics:  []string{embeddedTopic.Hex()},
			Data:    []byte{0xaa},
		}},
	}
	hookEvent := mustTypedEvent(t, hook)
	hookEvent.Attributes = append(hookEvent.Attributes, types.EventAttribute{Key: "msg_index", Value: "0"})

	resp, err := ParseResponse(&types.ExecTxResult{
		Code:    types.CodeTypeOK,
		GasUsed: 900,
		Events: []types.Event{
			bankTransferEvent("0"),
			hookEvent,
			bankReceivedEvent("0"),
		},
	})
	if err != nil {
		t.Fatalf("ParseResponse returned error: %v", err)
	}
	rawTx := cmtypes.Tx("cosmos transaction")
	blockHash := common.HexToHash("0xbeef")
	virtualTx, err := resp.SyntheticTx(TxContext{
		Tx:                      rawTx,
		EthereumMessageIndexes:  map[int]bool{},
		TotalMessages:           1,
		BlockHash:               blockHash,
		BlockNumber:             12,
		TxIndex:                 4,
		FirstLogIndex:           6,
		CumulativeGasUsedBefore: 100,
		ChainID:                 big.NewInt(1776),
	})
	if err != nil {
		t.Fatalf("SyntheticTx returned error: %v", err)
	}
	if virtualTx == nil {
		t.Fatal("expected synthetic transaction")
	}
	if virtualTx.Transaction.From != virtualibc.ContractAddress || virtualTx.Transaction.To == nil || *virtualTx.Transaction.To != target {
		t.Fatalf("unexpected transaction endpoints: from=%s to=%v", virtualTx.Transaction.From, virtualTx.Transaction.To)
	}
	if string(virtualTx.Transaction.Input) != string(hook.Input) || uint64(virtualTx.Transaction.Gas) != hook.GasUsed {
		t.Fatalf("unexpected transaction input/gas: input=%s gas=%d", hexutil.Encode(virtualTx.Transaction.Input), virtualTx.Transaction.Gas)
	}
	if virtualTx.Receipt.Status != ethtypes.ReceiptStatusSuccessful || virtualTx.Receipt.GasUsed != hook.GasUsed || virtualTx.Receipt.CumulativeGasUsed != 177 {
		t.Fatalf("unexpected receipt execution fields: %#v", virtualTx.Receipt)
	}

	logs := virtualTx.Receipt.Logs
	if len(logs) != 4 {
		t.Fatalf("expected bank, embedded, summary, bank logs; got %d", len(logs))
	}
	wantTopics := []common.Hash{virtualbank.TopicTransfer, embeddedTopic, virtualibc.TopicHookCall, virtualbank.TopicCoinReceived}
	for i, want := range wantTopics {
		if logs[i].Topics[0] != want || uint(logs[i].Index) != uint(6+i) {
			t.Fatalf("log %d mismatch: topic=%s index=%d", i, logs[i].Topics[0], logs[i].Index)
		}
		if !logs[i].Virtual || logs[i].CosmosHash == nil || logs[i].TxHash != virtualTx.Transaction.Hash {
			t.Fatalf("log %d missing shared synthetic identity: %#v", i, logs[i])
		}
	}
}

func TestFailedIBCHookBuildsFailedQueryableTx(t *testing.T) {
	hookEvent := mustTypedEvent(t, &evmtypes.EventIBCHookCall{
		DestinationPort:    "transfer",
		DestinationChannel: "channel-1",
		Sequence:           3,
		Contract:           "0x3333333333333333333333333333333333333333",
		Input:              []byte{0xca, 0xfe},
		Success:            false,
		ReturnData:         []byte{0x08, 0xc3},
		Error:              "execution reverted",
		GasUsed:            44,
	})
	hookEvent.Attributes = append(hookEvent.Attributes, types.EventAttribute{Key: "msg_index", Value: "0"})
	resp, err := ParseResponse(&types.ExecTxResult{Code: types.CodeTypeOK, Events: []types.Event{hookEvent}})
	if err != nil {
		t.Fatal(err)
	}
	virtualTx, err := resp.SyntheticTx(TxContext{
		Tx:            cmtypes.Tx("failed hook"),
		TotalMessages: 1,
		BlockHash:     common.HexToHash("0x44"),
		BlockNumber:   2,
		TxIndex:       1,
		FirstLogIndex: 5,
		ChainID:       big.NewInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if virtualTx.Receipt.Status != ethtypes.ReceiptStatusFailed || virtualTx.Receipt.VMError != "execution reverted" {
		t.Fatalf("unexpected failed receipt: %#v", virtualTx.Receipt)
	}
	if len(virtualTx.Receipt.Logs) != 1 || virtualTx.Receipt.Logs[0].Topics[0] != virtualibc.TopicHookCall {
		t.Fatalf("failed call summary log missing: %#v", virtualTx.Receipt.Logs)
	}
}

func bankTransferEvent(msgIndex string) types.Event {
	return types.Event{Type: virtualbank.EventTypeTransfer, Attributes: []types.EventAttribute{
		{Key: "sender", Value: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Key: "recipient", Value: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Key: "amount", Value: "5inj"},
		{Key: virtualbank.AttributeMsgIndex, Value: msgIndex},
	}}
}

func bankReceivedEvent(msgIndex string) types.Event {
	return types.Event{Type: virtualbank.EventTypeCoinReceived, Attributes: []types.EventAttribute{
		{Key: "receiver", Value: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Key: "amount", Value: "5inj"},
		{Key: virtualbank.AttributeMsgIndex, Value: msgIndex},
	}}
}

func mustTypedEvent(t *testing.T, event *evmtypes.EventIBCHookCall) types.Event {
	t.Helper()
	converted, err := sdk.TypedEventToEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return sdk.Events{converted}.ToABCIEvents()[0]
}
