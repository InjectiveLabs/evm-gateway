package ibc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	ibccoretypes "github.com/cosmos/ibc-go/v8/modules/core/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

const EventType = "injective.evm.v1.EventIBCHookCall"

var (
	// ContractAddress is the trusted EVM caller used by the IBC hook.
	ContractAddress = common.HexToAddress("0x0000000000000000000000000000000000000068")
	TopicHookCall   = crypto.Keccak256Hash([]byte("IBCHookCall(string,string,uint64,address,bool,bytes,string)"))

	summaryArgs = abi.Arguments{
		{Type: mustABIType("string")},
		{Type: mustABIType("string")},
		{Type: mustABIType("uint64")},
		{Type: mustABIType("bool")},
		{Type: mustABIType("bytes")},
		{Type: mustABIType("string")},
	}
)

// HookCall is the source-local representation of EventIBCHookCall.
type HookCall struct {
	DestinationPort    string
	DestinationChannel string
	Sequence           uint64
	Contract           common.Address
	Input              []byte
	Success            bool
	ReturnData         []byte
	Error              string
	GasUsed            uint64
	Logs               []*ethtypes.Log
	MsgIndex           *int
}

// ParseEvent parses normal and IBC callback-error-prefixed hook events.
func ParseEvent(event types.Event, eventIndex int) (*HookCall, bool, error) {
	normalized, matched := normalizeEvent(event)
	if !matched {
		return nil, false, nil
	}

	msgIndex, err := parseMsgIndex(normalized.Attributes)
	if err != nil {
		return nil, true, fmt.Errorf("IBC hook event %d invalid msg_index: %w", eventIndex, err)
	}

	parsed, err := sdk.ParseTypedEvent(normalized)
	if err != nil {
		return nil, true, fmt.Errorf("IBC hook event %d: %w", eventIndex, err)
	}

	typed, ok := parsed.(*evmtypes.EventIBCHookCall)
	if !ok {
		return nil, true, fmt.Errorf("IBC hook event %d decoded as %T", eventIndex, parsed)
	}

	if !common.IsHexAddress(typed.Contract) {
		return nil, true, fmt.Errorf("IBC hook event %d invalid contract %q", eventIndex, typed.Contract)
	}

	logs := make([]*ethtypes.Log, 0, len(typed.Logs))
	for logIndex, log := range typed.Logs {
		converted, err := ethereumLog(log)
		if err != nil {
			return nil, true, fmt.Errorf("IBC hook event %d log %d: %w", eventIndex, logIndex, err)
		}

		logs = append(logs, converted)
	}

	return &HookCall{
		DestinationPort:    typed.DestinationPort,
		DestinationChannel: typed.DestinationChannel,
		Sequence:           typed.Sequence,
		Contract:           common.HexToAddress(typed.Contract),
		Input:              append([]byte(nil), typed.Input...),
		Success:            typed.Success,
		ReturnData:         append([]byte(nil), typed.ReturnData...),
		Error:              typed.Error,
		GasUsed:            typed.GasUsed,
		Logs:               logs,
		MsgIndex:           msgIndex,
	}, true, nil
}

// EthereumLogs returns successful contract logs followed by the always-present
// IBC hook summary log.
func EthereumLogs(call *HookCall) ([]*ethtypes.Log, error) {
	if call == nil {
		return nil, nil
	}

	data, err := summaryArgs.Pack(
		call.DestinationPort,
		call.DestinationChannel,
		call.Sequence,
		call.Success,
		call.ReturnData,
		call.Error,
	)
	if err != nil {
		return nil, err
	}

	logs := make([]*ethtypes.Log, 0, len(call.Logs)+1)
	for _, log := range call.Logs {
		if log == nil {
			continue
		}
		logs = append(logs, &ethtypes.Log{
			Address: log.Address,
			Topics:  append([]common.Hash(nil), log.Topics...),
			Data:    append([]byte(nil), log.Data...),
		})
	}

	logs = append(logs, &ethtypes.Log{
		Address: ContractAddress,
		Topics:  []common.Hash{TopicHookCall, common.BytesToHash(call.Contract.Bytes())},
		Data:    data,
	})

	return logs, nil
}

func normalizeEvent(event types.Event) (types.Event, bool) {
	switch event.Type {
	case EventType:
		return event, true
	case ibccoretypes.ErrorAttributeKeyPrefix + EventType:
		attrs := make([]types.EventAttribute, len(event.Attributes))
		for i, attr := range event.Attributes {
			attrs[i] = types.EventAttribute{
				Key:   strings.TrimPrefix(attr.Key, ibccoretypes.ErrorAttributeKeyPrefix),
				Value: attr.Value,
				Index: attr.Index,
			}
		}
		return types.Event{Type: EventType, Attributes: attrs}, true
	default:
		return types.Event{}, false
	}
}

func parseMsgIndex(attrs []types.EventAttribute) (*int, error) {
	for _, attr := range attrs {
		if attr.Key != "msg_index" || strings.TrimSpace(attr.Value) == "" {
			continue
		}
		idx, err := strconv.Atoi(attr.Value)
		if err != nil {
			return nil, err
		}

		if idx < 0 {
			return nil, fmt.Errorf("negative msg_index %d", idx)
		}

		return &idx, nil
	}

	return nil, nil
}

func ethereumLog(log *evmtypes.Log) (*ethtypes.Log, error) {
	if log == nil {
		return nil, fmt.Errorf("nil log")
	}

	if !common.IsHexAddress(log.Address) {
		return nil, fmt.Errorf("invalid address %q", log.Address)
	}

	topics := make([]common.Hash, len(log.Topics))
	for i, topic := range log.Topics {
		bz, err := hexutil.Decode(topic)
		if err != nil || len(bz) != common.HashLength {
			return nil, fmt.Errorf("invalid topic %d %q", i, topic)
		}

		topics[i] = common.BytesToHash(bz)
	}

	return &ethtypes.Log{
		Address: common.HexToAddress(log.Address),
		Topics:  topics,
		Data:    append([]byte(nil), log.Data...),
	}, nil
}

func mustABIType(name string) abi.Type {
	t, err := abi.NewType(name, "", nil)
	if err != nil {
		panic(err)
	}

	return t
}
