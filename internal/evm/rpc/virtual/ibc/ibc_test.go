package ibc

import (
	"math/big"
	"strings"
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	ibccoretypes "github.com/cosmos/ibc-go/v8/modules/core/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

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
	// Decode with a client ABI, without relying on gateway-specific JSON fields.
	clientABI, err := abi.JSON(strings.NewReader(`[{"type":"event","name":"IBCHookCall","inputs":[
		{"name":"destinationPort","type":"string"},
		{"name":"destinationChannel","type":"string"},
		{"name":"sequence","type":"uint64"},
		{"name":"contractAddress","type":"address","indexed":true},
		{"name":"success","type":"bool"},
		{"name":"returnData","type":"bytes"},
		{"name":"errorMessage","type":"string"},
		{"name":"msgIndex","type":"uint256","indexed":true}
	]}]`))
	require.NoError(t, err)
	clientEvent := clientABI.Events["IBCHookCall"]
	require.Equal(t, clientEvent.ID, logs[1].Topics[0])
	indexed := abi.Arguments{clientEvent.Inputs[3], clientEvent.Inputs[7]}
	decoded := make(map[string]interface{})
	require.NoError(t, abi.ParseTopicsIntoMap(decoded, indexed, logs[1].Topics[1:]))
	require.Equal(t, target, decoded["contractAddress"])
	require.Equal(t, big.NewInt(3), decoded["msgIndex"])
	require.NoError(t, clientABI.UnpackIntoMap(decoded, "IBCHookCall", logs[1].Data))
	require.Equal(t, "transfer", decoded["destinationPort"])
	require.Equal(t, true, decoded["success"])
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
	require.Len(t, logs[0].Topics, 3)
	require.Equal(t, common.Hash{}, logs[0].Topics[2])
}

func TestHookLogsRequireMessageIndex(t *testing.T) {
	for _, index := range []string{"", "-1", "invalid"} {
		t.Run(index, func(t *testing.T) {
			event := typedEvent(t, &evmtypes.EventIBCHookCall{
				Contract: "0x1111111111111111111111111111111111111111",
			})
			if index != "" {
				event.Attributes = append(event.Attributes, types.EventAttribute{Key: "msg_index", Value: index})
			}
			call, matched, err := ParseEvent(event, 0)
			require.True(t, matched)
			if err == nil {
				_, err = EthereumLogs(call)
			}
			require.ErrorContains(t, err, "msg_index")
		})
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
