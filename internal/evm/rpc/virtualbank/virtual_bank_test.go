package virtualbank

import (
	"math/big"
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// TestParseEventsAndBuildLogsForTrackedBankEvents verifies tracked x/bank
// events are normalized and emitted as virtual Ethereum logs.
func TestParseEventsAndBuildLogsForTrackedBankEvents(t *testing.T) {
	addr20 := "0x1111111111111111111111111111111111111111"
	addr32Bz := []byte("12345678901234567890123456789012")
	addr32, err := sdkbech32.ConvertAndEncode("inj", addr32Bz)
	if err != nil {
		t.Fatalf("ConvertAndEncode returned error: %v", err)
	}

	events, err := ParseEvents([]types.Event{
		{
			Type: EventTypeTransfer,
			Attributes: []types.EventAttribute{
				{Key: "sender", Value: addr20},
				{Key: "recipient", Value: addr32},
				{Key: "amount", Value: "100inj"},
				{Key: AttributeMsgIndex, Value: "2"},
				{Key: AttributeMode, Value: ModeBeginBlock},
			},
		},
		{
			Type: EventTypeCoinSpent,
			Attributes: []types.EventAttribute{
				{Key: "spender", Value: addr20},
				{Key: "amount", Value: "7inj"},
			},
		},
		{
			Type: EventTypeCoinReceived,
			Attributes: []types.EventAttribute{
				{Key: "receiver", Value: addr20},
				{Key: "amount", Value: "8inj"},
			},
		},
		{
			Type: EventTypeCoinbase,
			Attributes: []types.EventAttribute{
				{Key: "minter", Value: addr20},
				{Key: "amount", Value: "9inj"},
			},
		},
		{
			Type: EventTypeBurn,
			Attributes: []types.EventAttribute{
				{Key: "burner", Value: addr20},
				{Key: "amount", Value: "10inj"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ParseEvents returned error: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("unexpected event count: got %d want 5", len(events))
	}
	if events[0].MsgIndex == nil || *events[0].MsgIndex != 2 {
		t.Fatalf("unexpected msg index: %#v", events[0].MsgIndex)
	}
	if events[0].Mode != ModeBeginBlock {
		t.Fatalf("unexpected mode: got %q want %q", events[0].Mode, ModeBeginBlock)
	}
	if events[0].Sender != common.BytesToHash(common.HexToAddress(addr20).Bytes()) {
		t.Fatalf("20-byte sender was not right-aligned into bytes32")
	}
	if events[0].Recipient != common.BytesToHash(addr32Bz) {
		t.Fatalf("32-byte bech32 recipient changed during bytes32 conversion")
	}

	logs, err := Logs(events, LogContext{
		BlockHash:     common.HexToHash("0xbeef"),
		BlockNumber:   12,
		TxHash:        common.HexToHash("0xabcd"),
		TxIndex:       3,
		FirstLogIndex: 4,
	})
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	if len(logs) != 5 {
		t.Fatalf("unexpected log count: got %d want 5", len(logs))
	}

	wantTopics := []common.Hash{TopicTransfer, TopicCoinSpent, TopicCoinReceived, TopicCoinbase, TopicBurn}
	for i, log := range logs {
		if log.Address != ContractAddress {
			t.Fatalf("log %d unexpected address: %s", i, log.Address.Hex())
		}
		if !log.Virtual {
			t.Fatalf("log %d expected virtual metadata", i)
		}
		if log.Topics[0] != wantTopics[i] {
			t.Fatalf("log %d unexpected topic0: got %s want %s", i, log.Topics[0].Hex(), wantTopics[i].Hex())
		}
		if uint64(log.BlockNumber) != 12 || uint(log.TxIndex) != 3 || uint(log.Index) != uint(4+i) {
			t.Fatalf("log %d unexpected location: block=%d tx=%d index=%d", i, log.BlockNumber, log.TxIndex, log.Index)
		}
	}

	values := unpackDenomAmount(t, logs[0].Data)
	if values[0].(string) != "inj" {
		t.Fatalf("unexpected denom: %v", values[0])
	}
	if values[1].(*big.Int).Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("unexpected amount: %v", values[1])
	}
}

// TestSplitBlockEventsUsesModeAttribute verifies finalize-block events are
// split by their begin/end block mode attribute.
func TestSplitBlockEventsUsesModeAttribute(t *testing.T) {
	begin, end, err := SplitBlockEvents([]types.Event{
		{
			Type: EventTypeCoinReceived,
			Attributes: []types.EventAttribute{
				{Key: "receiver", Value: "0x1111111111111111111111111111111111111111"},
				{Key: "amount", Value: "1inj"},
				{Key: AttributeMode, Value: ModeBeginBlock},
			},
		},
		{
			Type: EventTypeBurn,
			Attributes: []types.EventAttribute{
				{Key: "burner", Value: "0x2222222222222222222222222222222222222222"},
				{Key: "amount", Value: "2inj"},
				{Key: AttributeMode, Value: ModeEndBlock},
			},
		},
	})
	if err != nil {
		t.Fatalf("SplitBlockEvents returned error: %v", err)
	}
	if len(begin) != 1 || begin[0].Type != EventTypeCoinReceived {
		t.Fatalf("unexpected begin events: %#v", begin)
	}
	if len(end) != 1 || end[0].Type != EventTypeBurn {
		t.Fatalf("unexpected end events: %#v", end)
	}
}

// TestAddressBytes32RejectsOversizedRawAddress verifies raw address strings
// cannot exceed the bytes32 virtual log ABI field.
func TestAddressBytes32RejectsOversizedRawAddress(t *testing.T) {
	if _, err := AddressBytes32("123456789012345678901234567890123"); err == nil {
		t.Fatal("expected oversized raw address to fail")
	}
}

// unpackDenomAmount decodes the ABI payload used by virtual bank log data.
func unpackDenomAmount(t *testing.T, data []byte) []interface{} {
	t.Helper()
	stringType, err := abi.NewType("string", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	uintType, err := abi.NewType("uint256", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	args := abi.Arguments{{Type: stringType}, {Type: uintType}}
	values, err := args.Unpack(data)
	if err != nil {
		t.Fatalf("unpack log data: %v", err)
	}
	return values
}
