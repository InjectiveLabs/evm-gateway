package bank

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	sdkmath "cosmossdk.io/math"
	"github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	EventTypeTransfer     = "transfer"
	EventTypeCoinSpent    = "coin_spent"
	EventTypeCoinReceived = "coin_received"
	EventTypeCoinbase     = "coinbase"
	EventTypeBurn         = "burn"

	AttributeMsgIndex = "msg_index"
	AttributeMode     = "mode"

	ModeBeginBlock = "BeginBlock"
	ModeEndBlock   = "EndBlock"
)

var (
	// ContractAddress is the reserved pseudo-address used as the emitter of all
	// virtual native bank transfer logs.
	ContractAddress = common.HexToAddress("0x0000000000000000000000000000000000000800")

	TopicTransfer     = crypto.Keccak256Hash([]byte("NativeBankTransfer(bytes32,bytes32,string,uint256)"))
	TopicCoinSpent    = crypto.Keccak256Hash([]byte("NativeBankCoinSpent(bytes32,string,uint256)"))
	TopicCoinReceived = crypto.Keccak256Hash([]byte("NativeBankCoinReceived(bytes32,string,uint256)"))
	TopicCoinbase     = crypto.Keccak256Hash([]byte("NativeBankCoinbase(bytes32,string,uint256)"))
	TopicBurn         = crypto.Keccak256Hash([]byte("NativeBankBurn(bytes32,string,uint256)"))

	denomAmountArgs = abi.Arguments{
		{Type: mustABIType("string")},
		{Type: mustABIType("uint256")},
	}
)

// Event is the normalized form of a Cosmos x/bank event.
type Event struct {
	Type      string
	Sender    common.Hash
	Recipient common.Hash
	Actor     common.Hash
	Denom     string
	Amount    *big.Int
	MsgIndex  *int
	Mode      string
}

// ParseEvent parses one tracked x/bank ABCI event and expands a multi-coin
// amount into one normalized event per coin.
func ParseEvent(event types.Event, eventIndex int) ([]Event, bool, error) {
	if !IsTrackedEventType(event.Type) {
		return nil, false, nil
	}

	attrs := eventAttrs(event.Attributes)
	amountRaw, ok := attrs["amount"]
	if !ok {
		return nil, true, fmt.Errorf("%s event %d missing amount", event.Type, eventIndex)
	}

	coins, err := sdk.ParseCoinsNormalized(amountRaw)
	if err != nil {
		return nil, true, fmt.Errorf("%s event %d invalid amount %q: %w", event.Type, eventIndex, amountRaw, err)
	}

	msgIndex, err := parseMsgIndex(attrs)
	if err != nil {
		return nil, true, fmt.Errorf("%s event %d invalid msg_index: %w", event.Type, eventIndex, err)
	}

	base := Event{Type: event.Type, MsgIndex: msgIndex, Mode: attrs[AttributeMode]}
	switch event.Type {
	case EventTypeTransfer:
		base.Sender, err = requiredAddress(attrs, "sender", event.Type, eventIndex)
		if err == nil {
			base.Recipient, err = requiredAddress(attrs, "recipient", event.Type, eventIndex)
		}
	case EventTypeCoinSpent:
		base.Actor, err = requiredAddress(attrs, "spender", event.Type, eventIndex)
	case EventTypeCoinReceived:
		base.Actor, err = requiredAddress(attrs, "receiver", event.Type, eventIndex)
	case EventTypeCoinbase:
		base.Actor, err = requiredAddress(attrs, "minter", event.Type, eventIndex)
	case EventTypeBurn:
		base.Actor, err = requiredAddress(attrs, "burner", event.Type, eventIndex)
	}

	if err != nil {
		return nil, true, err
	}

	out := make([]Event, 0, len(coins))
	for _, coin := range coins {
		ev := base
		ev.Denom = coin.Denom
		ev.Amount = coinAmountBigInt(coin.Amount)
		out = append(out, ev)
	}

	return out, true, nil
}

// ParseEvents extracts all tracked x/bank events.
func ParseEvents(events []types.Event) ([]Event, error) {
	out := make([]Event, 0)
	for i, event := range events {
		parsed, matched, err := ParseEvent(event, i)
		if err != nil {
			return nil, err
		}

		if matched {
			out = append(out, parsed...)
		}
	}

	return out, nil
}

// SplitBlockEvents separates FinalizeBlock x/bank events by execution mode.
func SplitBlockEvents(events []types.Event) (begin []Event, end []Event, err error) {
	parsed, err := ParseEvents(events)
	if err != nil {
		return nil, nil, err
	}

	for _, event := range parsed {
		if event.Mode == ModeBeginBlock {
			begin = append(begin, event)
		} else {
			end = append(end, event)
		}
	}

	return begin, end, nil
}

// EthereumLogs converts normalized bank events into metadata-free Ethereum
// logs. The parent virtual package assigns block and transaction metadata.
func EthereumLogs(events []Event) ([]*ethtypes.Log, error) {
	logs := make([]*ethtypes.Log, 0, len(events))
	for _, event := range events {
		data, err := denomAmountArgs.Pack(event.Denom, event.Amount)
		if err != nil {
			return nil, err
		}

		topics, err := eventTopics(event)
		if err != nil {
			return nil, err
		}
		logs = append(logs, &ethtypes.Log{Address: ContractAddress, Topics: topics, Data: data})
	}

	return logs, nil
}

func IsTrackedEventType(eventType string) bool {
	switch eventType {
	case EventTypeTransfer, EventTypeCoinSpent, EventTypeCoinReceived, EventTypeCoinbase, EventTypeBurn:
		return true
	default:
		return false
	}
}

func AddressBytes32(value string) (common.Hash, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return common.Hash{}, fmt.Errorf("empty address")
	}

	if bz, ok, err := decodeHexAddress(value); ok || err != nil {
		if err != nil {
			return common.Hash{}, err
		}

		return bytesToHash32(bz)
	}

	if _, bz, err := sdkbech32.DecodeAndConvert(value); err == nil {
		return bytesToHash32(bz)
	}

	return bytesToHash32([]byte(value))
}

func eventAttrs(attrs []types.EventAttribute) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.Key] = attr.Value
	}

	return out
}

func parseMsgIndex(attrs map[string]string) (*int, error) {
	raw, ok := attrs[AttributeMsgIndex]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	idx, err := strconv.Atoi(raw)
	if err != nil {
		return nil, err
	}

	if idx < 0 {
		return nil, fmt.Errorf("negative msg_index %d", idx)
	}

	return &idx, nil
}

func requiredAddress(attrs map[string]string, key, eventType string, eventIndex int) (common.Hash, error) {
	raw, ok := attrs[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return common.Hash{}, fmt.Errorf("%s event %d missing %s", eventType, eventIndex, key)
	}

	value, err := AddressBytes32(raw)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%s event %d invalid %s %q: %w", eventType, eventIndex, key, raw, err)
	}

	return value, nil
}

func coinAmountBigInt(amount sdkmath.Int) *big.Int {
	return new(big.Int).Set(amount.BigInt())
}

func decodeHexAddress(value string) ([]byte, bool, error) {
	raw := value
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		bz, err := hexutil.Decode(raw)
		return bz, true, err
	}

	if len(raw)%2 != 0 || !isHex(raw) {
		return nil, false, nil
	}

	bz, err := hexutil.Decode("0x" + raw)
	return bz, true, err
}

func isHex(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}

	return true
}

func bytesToHash32(bz []byte) (common.Hash, error) {
	if len(bz) > common.HashLength {
		return common.Hash{}, fmt.Errorf("address is %d bytes, max %d", len(bz), common.HashLength)
	}

	return common.BytesToHash(bz), nil
}

func eventTopics(event Event) ([]common.Hash, error) {
	switch event.Type {
	case EventTypeTransfer:
		return []common.Hash{TopicTransfer, event.Sender, event.Recipient}, nil
	case EventTypeCoinSpent:
		return []common.Hash{TopicCoinSpent, event.Actor}, nil
	case EventTypeCoinReceived:
		return []common.Hash{TopicCoinReceived, event.Actor}, nil
	case EventTypeCoinbase:
		return []common.Hash{TopicCoinbase, event.Actor}, nil
	case EventTypeBurn:
		return []common.Hash{TopicBurn, event.Actor}, nil
	default:
		return nil, fmt.Errorf("unsupported event type %q", event.Type)
	}
}

func mustABIType(name string) abi.Type {
	t, err := abi.NewType(name, "", nil)
	if err != nil {
		panic(err)
	}

	return t
}
