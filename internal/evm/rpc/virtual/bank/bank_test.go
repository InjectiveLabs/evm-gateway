package bank

import (
	"math/big"
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
)

func TestParseEventAndEthereumLogs(t *testing.T) {
	events, matched, err := ParseEvent(types.Event{
		Type: EventTypeTransfer,
		Attributes: []types.EventAttribute{
			{Key: "sender", Value: "0x1111111111111111111111111111111111111111"},
			{Key: "recipient", Value: "0x2222222222222222222222222222222222222222"},
			{Key: "amount", Value: "7inj,9usdt"},
			{Key: AttributeMsgIndex, Value: "2"},
		},
	}, 0)
	if err != nil {
		t.Fatalf("ParseEvent returned error: %v", err)
	}
	if !matched || len(events) != 2 {
		t.Fatalf("unexpected parse result: matched=%t events=%d", matched, len(events))
	}
	if events[0].MsgIndex == nil || *events[0].MsgIndex != 2 {
		t.Fatalf("unexpected msg index: %v", events[0].MsgIndex)
	}
	if events[0].Denom != "inj" || events[0].Amount.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("unexpected first coin: %s %s", events[0].Denom, events[0].Amount)
	}

	logs, err := EthereumLogs(events)
	if err != nil {
		t.Fatalf("EthereumLogs returned error: %v", err)
	}
	if len(logs) != 2 || logs[0].Address != ContractAddress || logs[0].Topics[0] != TopicTransfer {
		t.Fatalf("unexpected logs: %#v", logs)
	}
	if logs[0].Topics[1] != common.HexToHash("0x1111111111111111111111111111111111111111") {
		t.Fatalf("unexpected sender topic: %s", logs[0].Topics[1])
	}
}

func TestAddressBytes32RejectsOversizedRawAddress(t *testing.T) {
	if _, err := AddressBytes32("123456789012345678901234567890123"); err == nil {
		t.Fatal("expected oversized raw address to fail")
	}
}

func TestParseEventsSkipsEmptyAmounts(t *testing.T) {
	events, err := ParseEvents([]types.Event{
		{Type: EventTypeCoinSpent, Attributes: []types.EventAttribute{
			{Key: "spender", Value: "0x1111111111111111111111111111111111111111"},
			{Key: "amount", Value: ""},
		}},
		{Type: EventTypeCoinReceived, Attributes: []types.EventAttribute{
			{Key: "receiver", Value: "0x1111111111111111111111111111111111111111"},
			{Key: "amount", Value: "8inj"},
		}},
	})
	if err != nil {
		t.Fatalf("ParseEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Type != EventTypeCoinReceived {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestParseEventsRejectsMissingAmount(t *testing.T) {
	if _, err := ParseEvents([]types.Event{{
		Type:       EventTypeBurn,
		Attributes: []types.EventAttribute{{Key: "burner", Value: "0x1111111111111111111111111111111111111111"}},
	}}); err == nil {
		t.Fatal("expected missing amount to fail")
	}
}

func TestSplitBlockEventsUsesMode(t *testing.T) {
	begin, end, err := SplitBlockEvents([]types.Event{
		{Type: EventTypeCoinReceived, Attributes: []types.EventAttribute{
			{Key: "receiver", Value: "0x1111111111111111111111111111111111111111"},
			{Key: "amount", Value: "1inj"},
			{Key: AttributeMode, Value: ModeBeginBlock},
		}},
		{Type: EventTypeBurn, Attributes: []types.EventAttribute{
			{Key: "burner", Value: "0x2222222222222222222222222222222222222222"},
			{Key: "amount", Value: "2inj"},
			{Key: AttributeMode, Value: ModeEndBlock},
		}},
	})
	if err != nil {
		t.Fatalf("SplitBlockEvents returned error: %v", err)
	}
	if len(begin) != 1 || begin[0].Type != EventTypeCoinReceived || len(end) != 1 || end[0].Type != EventTypeBurn {
		t.Fatalf("unexpected split: begin=%#v end=%#v", begin, end)
	}
}
