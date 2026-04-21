package virtualbank

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	sdkmath "cosmossdk.io/math"
	"github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
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

	blockPhaseBegin = "begin_block"
	blockPhaseEnd   = "end_block"
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

type TransferEvent struct {
	Type      string
	Sender    common.Hash
	Recipient common.Hash
	Actor     common.Hash
	Denom     string
	Amount    *big.Int
	MsgIndex  *int
	Mode      string
}

type LogContext struct {
	BlockHash     common.Hash
	BlockNumber   uint64
	TxHash        common.Hash
	TxIndex       uint
	FirstLogIndex uint
	CosmosHash    *common.Hash
}

// RPCLog matches the Ethereum log JSON shape and carries optional metadata for
// synthesized Cosmos events.
type RPCLog struct {
	Address     common.Address
	Topics      []common.Hash
	Data        []byte
	BlockNumber uint64
	TxHash      common.Hash
	TxIndex     uint
	BlockHash   common.Hash
	Index       uint
	Removed     bool
	Virtual     bool
	CosmosHash  *common.Hash
}

type rpcLogJSON struct {
	Address     common.Address `json:"address"`
	Topics      []common.Hash  `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	TxHash      common.Hash    `json:"transactionHash"`
	TxIndex     hexutil.Uint   `json:"transactionIndex"`
	BlockHash   common.Hash    `json:"blockHash"`
	Index       hexutil.Uint   `json:"logIndex"`
	Removed     bool           `json:"removed"`
	Virtual     bool           `json:"virtual,omitempty"`
	CosmosHash  *common.Hash   `json:"cosmos_hash,omitempty"`
}

func (l RPCLog) MarshalJSON() ([]byte, error) {
	return json.Marshal(rpcLogJSON{
		Address:     l.Address,
		Topics:      l.Topics,
		Data:        hexutil.Bytes(l.Data),
		BlockNumber: hexutil.Uint64(l.BlockNumber),
		TxHash:      l.TxHash,
		TxIndex:     hexutil.Uint(l.TxIndex),
		BlockHash:   l.BlockHash,
		Index:       hexutil.Uint(l.Index),
		Removed:     l.Removed,
		Virtual:     l.Virtual,
		CosmosHash:  l.CosmosHash,
	})
}

func (l *RPCLog) UnmarshalJSON(input []byte) error {
	var dec rpcLogJSON
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	l.Address = dec.Address
	l.Topics = append([]common.Hash(nil), dec.Topics...)
	l.Data = append([]byte(nil), dec.Data...)
	l.BlockNumber = uint64(dec.BlockNumber)
	l.TxHash = dec.TxHash
	l.TxIndex = uint(dec.TxIndex)
	l.BlockHash = dec.BlockHash
	l.Index = uint(dec.Index)
	l.Removed = dec.Removed
	l.Virtual = dec.Virtual
	l.CosmosHash = copyHashPtr(dec.CosmosHash)
	return nil
}

func mustABIType(name string) abi.Type {
	t, err := abi.NewType(name, "", nil)
	if err != nil {
		panic(err)
	}
	return t
}

// ParseEvents extracts the tracked x/bank transfer events and expands each SDK
// coin in the amount attribute into an individual virtual event.
func ParseEvents(events []types.Event) ([]TransferEvent, error) {
	out := make([]TransferEvent, 0)
	for eventIndex, event := range events {
		if !IsTrackedEventType(event.Type) {
			continue
		}

		attrs := eventAttrs(event.Attributes)
		amountRaw, ok := attrs["amount"]
		if !ok || strings.TrimSpace(amountRaw) == "" {
			return nil, fmt.Errorf("%s event %d missing amount", event.Type, eventIndex)
		}
		coins, err := sdk.ParseCoinsNormalized(amountRaw)
		if err != nil {
			return nil, fmt.Errorf("%s event %d invalid amount %q: %w", event.Type, eventIndex, amountRaw, err)
		}
		msgIndex, err := parseMsgIndex(attrs)
		if err != nil {
			return nil, fmt.Errorf("%s event %d invalid msg_index: %w", event.Type, eventIndex, err)
		}

		base := TransferEvent{Type: event.Type, MsgIndex: msgIndex, Mode: attrs[AttributeMode]}
		switch event.Type {
		case EventTypeTransfer:
			sender, err := requiredAddress(attrs, "sender", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			recipient, err := requiredAddress(attrs, "recipient", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			base.Sender = sender
			base.Recipient = recipient
		case EventTypeCoinSpent:
			actor, err := requiredAddress(attrs, "spender", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			base.Actor = actor
		case EventTypeCoinReceived:
			actor, err := requiredAddress(attrs, "receiver", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			base.Actor = actor
		case EventTypeCoinbase:
			actor, err := requiredAddress(attrs, "minter", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			base.Actor = actor
		case EventTypeBurn:
			actor, err := requiredAddress(attrs, "burner", event.Type, eventIndex)
			if err != nil {
				return nil, err
			}
			base.Actor = actor
		}

		for _, coin := range coins {
			ev := base
			ev.Denom = coin.Denom
			ev.Amount = coinAmountBigInt(coin.Amount)
			out = append(out, ev)
		}
	}
	return out, nil
}

func SplitBlockEvents(events []types.Event) (begin []TransferEvent, end []TransferEvent, err error) {
	parsed, err := ParseEvents(events)
	if err != nil {
		return nil, nil, err
	}
	for _, event := range parsed {
		if event.Mode == ModeBeginBlock {
			begin = append(begin, event)
			continue
		}
		end = append(end, event)
	}
	return begin, end, nil
}

func IsTrackedEventType(eventType string) bool {
	switch eventType {
	case EventTypeTransfer, EventTypeCoinSpent, EventTypeCoinReceived, EventTypeCoinbase, EventTypeBurn:
		return true
	default:
		return false
	}
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

func requiredAddress(attrs map[string]string, key string, eventType string, eventIndex int) (common.Hash, error) {
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

// AddressBytes32 converts bech32, hex, or short raw address strings into the
// bytes32 representation used by the virtual log ABI. Shorter values are
// right-aligned, matching EVM topic encoding for bytes32.
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

func Logs(events []TransferEvent, ctx LogContext) ([]*RPCLog, error) {
	logs := make([]*RPCLog, 0, len(events))
	for i, event := range events {
		data, err := denomAmountArgs.Pack(event.Denom, event.Amount)
		if err != nil {
			return nil, err
		}

		topics, err := eventTopics(event)
		if err != nil {
			return nil, err
		}

		logs = append(logs, &RPCLog{
			Address:     ContractAddress,
			Topics:      topics,
			Data:        data,
			BlockNumber: ctx.BlockNumber,
			TxHash:      ctx.TxHash,
			TxIndex:     ctx.TxIndex,
			BlockHash:   ctx.BlockHash,
			Index:       ctx.FirstLogIndex + uint(i),
			Virtual:     true,
			CosmosHash:  copyHashPtr(ctx.CosmosHash),
		})
	}
	return logs, nil
}

func NewRPCLog(log *ethtypes.Log, virtual bool, cosmosHash *common.Hash) *RPCLog {
	if log == nil {
		return nil
	}
	return &RPCLog{
		Address:     log.Address,
		Topics:      append([]common.Hash(nil), log.Topics...),
		Data:        append([]byte(nil), log.Data...),
		BlockNumber: log.BlockNumber,
		TxHash:      log.TxHash,
		TxIndex:     log.TxIndex,
		BlockHash:   log.BlockHash,
		Index:       log.Index,
		Removed:     log.Removed,
		Virtual:     virtual,
		CosmosHash:  copyHashPtr(cosmosHash),
	}
}

func WrapLogs(logs []*ethtypes.Log, virtual bool, cosmosHash *common.Hash) []*RPCLog {
	if logs == nil {
		return nil
	}
	out := make([]*RPCLog, 0, len(logs))
	for _, log := range logs {
		out = append(out, NewRPCLog(log, virtual, cosmosHash))
	}
	return out
}

func EthLog(log *RPCLog) *ethtypes.Log {
	if log == nil {
		return nil
	}
	return &ethtypes.Log{
		Address:     log.Address,
		Topics:      append([]common.Hash(nil), log.Topics...),
		Data:        append([]byte(nil), log.Data...),
		BlockNumber: log.BlockNumber,
		TxHash:      log.TxHash,
		TxIndex:     log.TxIndex,
		BlockHash:   log.BlockHash,
		Index:       log.Index,
		Removed:     log.Removed,
	}
}

func EthLogs(logs []*RPCLog) []*ethtypes.Log {
	if logs == nil {
		return nil
	}
	out := make([]*ethtypes.Log, 0, len(logs))
	for _, log := range logs {
		if ethLog := EthLog(log); ethLog != nil {
			out = append(out, ethLog)
		}
	}
	return out
}

func FlattenEthLogs(groups [][]*RPCLog) []*ethtypes.Log {
	out := make([]*ethtypes.Log, 0)
	for _, group := range groups {
		out = append(out, EthLogs(group)...)
	}
	return out
}

func eventTopics(event TransferEvent) ([]common.Hash, error) {
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

func SetLogMetadata(logs []*RPCLog, ctx LogContext) {
	for i, log := range logs {
		if log == nil {
			continue
		}
		log.BlockNumber = ctx.BlockNumber
		log.TxHash = ctx.TxHash
		log.TxIndex = ctx.TxIndex
		log.BlockHash = ctx.BlockHash
		log.Index = ctx.FirstLogIndex + uint(i)
	}
}

func OriginalCosmosTxHash(tx cmtypes.Tx) common.Hash {
	return common.BytesToHash(tx.Hash())
}

func CosmosTxHash(tx cmtypes.Tx) common.Hash {
	original := OriginalCosmosTxHash(tx)
	return crypto.Keccak256Hash(original.Bytes())
}

func BeginBlockHash(height int64) common.Hash {
	return blockPhaseHash(blockPhaseBegin, height)
}

func EndBlockHash(height int64) common.Hash {
	return blockPhaseHash(blockPhaseEnd, height)
}

func blockPhaseHash(phase string, height int64) common.Hash {
	payload := make([]byte, len(phase)+8)
	copy(payload, phase)
	binary.BigEndian.PutUint64(payload[len(phase):], uint64(height))
	return crypto.Keccak256Hash(payload)
}

func NewRPCTransaction(hash common.Hash, blockHash common.Hash, blockNumber uint64, index uint64, chainID *big.Int, cosmosHash *common.Hash) *rpctypes.RPCTransaction {
	to := ContractAddress
	txIndex := hexutil.Uint64(index)
	zero := big.NewInt(0)

	var chainIDHex *hexutil.Big
	if chainID != nil {
		chainIDHex = (*hexutil.Big)(new(big.Int).Set(chainID))
	}

	return &rpctypes.RPCTransaction{
		BlockHash:        &blockHash,
		BlockNumber:      (*hexutil.Big)(new(big.Int).SetUint64(blockNumber)),
		From:             common.Address{},
		Gas:              hexutil.Uint64(0),
		GasPrice:         (*hexutil.Big)(new(big.Int).Set(zero)),
		Hash:             hash,
		Input:            hexutil.Bytes{},
		Nonce:            hexutil.Uint64(0),
		To:               &to,
		TransactionIndex: &txIndex,
		Value:            (*hexutil.Big)(new(big.Int).Set(zero)),
		Type:             hexutil.Uint64(ethtypes.LegacyTxType),
		ChainID:          chainIDHex,
		V:                (*hexutil.Big)(new(big.Int).Set(zero)),
		R:                (*hexutil.Big)(new(big.Int).Set(zero)),
		S:                (*hexutil.Big)(new(big.Int).Set(zero)),
		Virtual:          true,
		CosmosHash:       copyHashPtr(cosmosHash),
	}
}

func copyHashPtr(hash *common.Hash) *common.Hash {
	if hash == nil {
		return nil
	}
	value := *hash
	return &value
}

func EventsForMsg(events []TransferEvent, msgIndex int, totalMsgs int) []TransferEvent {
	out := make([]TransferEvent, 0)
	for _, event := range events {
		if event.MsgIndex != nil {
			if *event.MsgIndex == msgIndex {
				out = append(out, event)
			}
			continue
		}
		if totalMsgs == 1 && msgIndex == 0 {
			out = append(out, event)
		}
	}
	return out
}

func EventsForNonEthMessages(events []TransferEvent, ethMsgIndexes map[int]bool, totalMsgs int) []TransferEvent {
	out := make([]TransferEvent, 0)
	for _, event := range events {
		if event.MsgIndex != nil {
			if !ethMsgIndexes[*event.MsgIndex] {
				out = append(out, event)
			}
			continue
		}
		if totalMsgs == 1 && ethMsgIndexes[0] {
			continue
		}
		out = append(out, event)
	}
	return out
}
