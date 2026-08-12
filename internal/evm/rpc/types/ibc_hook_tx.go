package types

import (
	"fmt"
	"math/big"
	"strings"

	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/gogoproto/proto"
	channeltypes "github.com/cosmos/ibc-go/v8/modules/core/04-channel/types"
	coretypes "github.com/cosmos/ibc-go/v8/modules/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// IBCEVMHookTxEventType is the typed ABCI event name for IBC-triggered EVM hook executions.
var IBCEVMHookTxEventType = proto.MessageName(&evmtypes.EventIBCEVMHookTx{})

// IBCErrorEventPrefix is added by IBC to callback events emitted while returning an error acknowledgement.
const IBCErrorEventPrefix = coretypes.ErrorAttributeKeyPrefix

// IBCEVMHookTxEventsByMsgIndex returns hook events keyed by the MsgRecvPacket
// message index that executed them. Matching by packet identity preserves the
// Ethereum-visible execution order when a Cosmos transaction mixes IBC and EVM
// messages.
func IBCEVMHookTxEventsByMsgIndex(tx sdk.Tx, txResult *abci.ExecTxResult) (map[int][]*evmtypes.EventIBCEVMHookTx, error) {
	if tx == nil || txResult == nil {
		return nil, nil
	}

	recvPacketMsgIndexes := make(map[ibcPacketKey]int)
	for msgIndex, msg := range tx.GetMsgs() {
		recvPacket, ok := msg.(*channeltypes.MsgRecvPacket)
		if !ok {
			continue
		}
		key := ibcPacketKeyFromPacket(recvPacket.Packet)
		if _, exists := recvPacketMsgIndexes[key]; !exists {
			recvPacketMsgIndexes[key] = msgIndex
		}
	}
	if len(recvPacketMsgIndexes) == 0 {
		return nil, nil
	}

	events, err := ibcEVMHookTxEventsFromResult(txResult)
	if err != nil || len(events) == 0 {
		return nil, err
	}

	eventsByMsgIndex := make(map[int][]*evmtypes.EventIBCEVMHookTx, len(events))
	for _, event := range events {
		key := ibcPacketKeyFromHookEvent(event)
		msgIndex, ok := recvPacketMsgIndexes[key]
		if !ok {
			return nil, fmt.Errorf(
				"IBC EVM hook event does not match a MsgRecvPacket: src %s/%s dst %s/%s seq %d",
				event.SourcePort,
				event.SourceChannel,
				event.DestinationPort,
				event.DestinationChannel,
				event.Sequence,
			)
		}
		eventsByMsgIndex[msgIndex] = append(eventsByMsgIndex[msgIndex], event)
	}

	return eventsByMsgIndex, nil
}

func ibcEVMHookTxEventsFromResult(txResult *abci.ExecTxResult) ([]*evmtypes.EventIBCEVMHookTx, error) {
	events := make([]*evmtypes.EventIBCEVMHookTx, 0)
	for _, event := range txResult.Events {
		eventCopy := event
		switch event.Type {
		case IBCEVMHookTxEventType:
		case IBCErrorEventPrefix + IBCEVMHookTxEventType:
			eventCopy.Type = IBCEVMHookTxEventType
			eventCopy.Attributes = append([]abci.EventAttribute(nil), event.Attributes...)
			for i := range eventCopy.Attributes {
				eventCopy.Attributes[i].Key = strings.TrimPrefix(eventCopy.Attributes[i].Key, IBCErrorEventPrefix)
			}
		default:
			continue
		}

		parsed, err := sdk.ParseTypedEvent(eventCopy)
		if err != nil {
			return nil, err
		}
		hookEvent, ok := parsed.(*evmtypes.EventIBCEVMHookTx)
		if !ok {
			return nil, fmt.Errorf("unexpected IBC EVM hook event type %T", parsed)
		}
		events = append(events, hookEvent)
	}
	return events, nil
}

type ibcPacketKey struct {
	sourcePort         string
	sourceChannel      string
	destinationPort    string
	destinationChannel string
	sequence           uint64
}

func ibcPacketKeyFromPacket(packet channeltypes.Packet) ibcPacketKey {
	return ibcPacketKey{
		sourcePort:         packet.SourcePort,
		sourceChannel:      packet.SourceChannel,
		destinationPort:    packet.DestinationPort,
		destinationChannel: packet.DestinationChannel,
		sequence:           packet.Sequence,
	}
}

func ibcPacketKeyFromHookEvent(event *evmtypes.EventIBCEVMHookTx) ibcPacketKey {
	return ibcPacketKey{
		sourcePort:         event.SourcePort,
		sourceChannel:      event.SourceChannel,
		destinationPort:    event.DestinationPort,
		destinationChannel: event.DestinationChannel,
		sequence:           event.Sequence,
	}
}

// NewIBCEVMHookRPCTransaction builds the JSON-RPC transaction representation
// for a synthetic IBC hook execution.
func NewIBCEVMHookRPCTransaction(
	event *evmtypes.EventIBCEVMHookTx,
	blockHash common.Hash,
	blockNumber uint64,
	index uint64,
	chainID *big.Int,
) *RPCTransaction {
	from := common.HexToAddress(event.From)
	to := common.HexToAddress(event.Contract)
	zero := big.NewInt(0)
	hash := common.HexToHash(event.Response.Hash)

	return &RPCTransaction{
		Type:             hexutil.Uint64(ethtypes.LegacyTxType),
		From:             from,
		Gas:              hexutil.Uint64(event.GasLimit),
		GasPrice:         (*hexutil.Big)(zero),
		Hash:             hash,
		Input:            hexutil.Bytes(event.Input),
		Nonce:            hexutil.Uint64(0),
		To:               &to,
		Value:            (*hexutil.Big)(zero),
		V:                (*hexutil.Big)(zero),
		R:                (*hexutil.Big)(zero),
		S:                (*hexutil.Big)(zero),
		BlockHash:        &blockHash,
		BlockNumber:      (*hexutil.Big)(new(big.Int).SetUint64(blockNumber)),
		TransactionIndex: (*hexutil.Uint64)(&index),
		ChainID:          (*hexutil.Big)(chainID),
	}
}

// IBCEVMHookLogs converts hook response logs to Ethereum logs at the supplied
// reconstructed block position.
func IBCEVMHookLogs(
	event *evmtypes.EventIBCEVMHookTx,
	blockNumber uint64,
	blockHash common.Hash,
	txIndex uint64,
	firstLogIndex uint,
) []*ethtypes.Log {
	response := event.GetResponse()
	if response == nil || len(response.Logs) == 0 {
		return nil
	}

	txHash := common.HexToHash(response.Hash)
	logs := make([]*ethtypes.Log, 0, len(response.Logs))
	for _, log := range response.Logs {
		if log == nil {
			continue
		}
		ethLog := log.ToEthereum()
		ethLog.TxHash = txHash
		ethLog.BlockNumber = blockNumber
		ethLog.TxIndex = uint(txIndex)
		ethLog.Index = firstLogIndex + uint(len(logs))
		ethLog.BlockHash = blockHash
		logs = append(logs, ethLog)
	}
	return logs
}
