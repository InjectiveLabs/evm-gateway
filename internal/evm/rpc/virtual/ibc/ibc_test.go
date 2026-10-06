package ibc

import (
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	ibccoretypes "github.com/cosmos/ibc-go/v8/modules/core/types"
	"github.com/ethereum/go-ethereum/common"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestParseEventAndEthereumLogs(t *testing.T) {
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	emitter := common.HexToAddress("0x2222222222222222222222222222222222222222")
	embeddedTopic := common.HexToHash("0x1234")
	event := typedEvent(t, &evmtypes.EventIBCHookCall{
		DestinationPort:    "transfer",
		DestinationChannel: "channel-7",
		Sequence:           42,
		Contract:           target.Hex(),
		Input:              []byte{0xde, 0xad},
		Success:            true,
		ReturnData:         []byte{0xbe, 0xef},
		GasUsed:            91,
		Logs: []*evmtypes.Log{{
			Address: emitter.Hex(),
			Topics:  []string{embeddedTopic.Hex()},
			Data:    []byte{0xaa},
		}},
	})
	event.Attributes = append(event.Attributes, types.EventAttribute{Key: "msg_index", Value: "3"})

	call, matched, err := ParseEvent(event, 0)
	if err != nil {
		t.Fatalf("ParseEvent returned error: %v", err)
	}
	if !matched || call.Contract != target || call.MsgIndex == nil || *call.MsgIndex != 3 {
		t.Fatalf("unexpected call: %#v", call)
	}

	logs, err := EthereumLogs(call)
	if err != nil {
		t.Fatalf("EthereumLogs returned error: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected embedded and summary logs, got %d", len(logs))
	}
	if logs[0].Address != emitter || logs[0].Topics[0] != embeddedTopic {
		t.Fatalf("unexpected embedded log: %#v", logs[0])
	}
	if logs[1].Address != ContractAddress || logs[1].Topics[0] != TopicHookCall || logs[1].Topics[1] != common.BytesToHash(target.Bytes()) {
		t.Fatalf("unexpected summary log: %#v", logs[1])
	}
	values, err := summaryArgs.Unpack(logs[1].Data)
	if err != nil {
		t.Fatalf("unpack summary data: %v", err)
	}
	if values[0] != "transfer" || values[1] != "channel-7" || values[2] != uint64(42) || values[3] != true {
		t.Fatalf("unexpected summary values: %#v", values)
	}
}

func TestParseCallbackErrorPrefixedEvent(t *testing.T) {
	event := typedEvent(t, &evmtypes.EventIBCHookCall{
		DestinationPort:    "transfer",
		DestinationChannel: "channel-2",
		Sequence:           8,
		Contract:           "0x3333333333333333333333333333333333333333",
		Success:            false,
		ReturnData:         []byte{0x08, 0xc3},
		Error:              "execution reverted",
		GasUsed:            55,
	})
	event.Type = ibccoretypes.ErrorAttributeKeyPrefix + event.Type
	for i := range event.Attributes {
		event.Attributes[i].Key = ibccoretypes.ErrorAttributeKeyPrefix + event.Attributes[i].Key
	}
	event.Attributes = append(event.Attributes, types.EventAttribute{
		Key:   ibccoretypes.ErrorAttributeKeyPrefix + "msg_index",
		Value: "0",
	})

	call, matched, err := ParseEvent(event, 0)
	if err != nil {
		t.Fatalf("ParseEvent returned error: %v", err)
	}
	if !matched || call.Success || call.Error != "execution reverted" || call.MsgIndex == nil || *call.MsgIndex != 0 {
		t.Fatalf("unexpected failed call: %#v", call)
	}
	logs, err := EthereumLogs(call)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Topics[0] != TopicHookCall {
		t.Fatalf("failed call must emit only its summary log: %#v", logs)
	}
}

func typedEvent(t *testing.T, event *evmtypes.EventIBCHookCall) types.Event {
	t.Helper()
	converted, err := sdk.TypedEventToEvent(event)
	if err != nil {
		t.Fatalf("TypedEventToEvent returned error: %v", err)
	}
	return sdk.Events{converted}.ToABCIEvents()[0]
}
