package indexer

import (
	"bytes"
	"fmt"
	"math/big"

	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	capnp "capnproto.org/go/capnp/v3"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer/kvcapnp"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

var kvCapnpMagic = []byte{0x89, 'e', 'g', 'c', 'p', '1', '\r', '\n'}

func newKVCapnpMessage() (*capnp.Message, *capnp.Segment) {
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		panic(err)
	}
	return msg, seg
}

func mustCapnpPayload(msg *capnp.Message) []byte {
	bz, err := msg.Marshal()
	if err != nil {
		panic(err)
	}
	out := make([]byte, 0, len(kvCapnpMagic)+len(bz))
	out = append(out, kvCapnpMagic...)
	out = append(out, bz...)
	return out
}

func capnpPayloadMessage(bz []byte) (*capnp.Message, bool, error) {
	if !bytes.HasPrefix(bz, kvCapnpMagic) {
		return nil, false, nil
	}
	msg, err := capnp.Unmarshal(bz[len(kvCapnpMagic):])
	return msg, true, err
}

func mustMarshalBlockMeta(meta CachedBlockMeta) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootBlockMeta(seg)
	if err != nil {
		panic(err)
	}
	root.SetHeight(meta.Height)
	mustSet(root.SetHash(meta.Hash))
	mustSet(root.SetParentHash(meta.ParentHash))
	mustSet(root.SetStateRoot(meta.StateRoot))
	mustSet(root.SetMiner(meta.Miner))
	root.SetTimestamp(meta.Timestamp)
	root.SetSize(meta.Size)
	root.SetGasLimit(meta.GasLimit)
	root.SetGasUsed(meta.GasUsed)
	root.SetEthTxCount(meta.EthTxCount)
	root.SetTxCount(meta.TxCount)
	mustSet(root.SetBloom(meta.Bloom))
	mustSet(root.SetTransactionsRoot(meta.TransactionsRoot))
	mustSet(root.SetBaseFee(meta.BaseFee))
	root.SetVirtualizedCosmosEvents(meta.VirtualizedCosmosEvents)
	return mustCapnpPayload(msg)
}

func unmarshalBlockMetaPayload(bz []byte) (CachedBlockMeta, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return CachedBlockMeta{}, err
	}
	if !ok {
		return unmarshalJSON[CachedBlockMeta](bz)
	}
	root, err := kvcapnp.ReadRootBlockMeta(msg)
	if err != nil {
		return CachedBlockMeta{}, err
	}
	hash, err := root.Hash()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	parentHash, err := root.ParentHash()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	stateRoot, err := root.StateRoot()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	miner, err := root.Miner()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	bloom, err := root.Bloom()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	transactionsRoot, err := root.TransactionsRoot()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	baseFee, err := root.BaseFee()
	if err != nil {
		return CachedBlockMeta{}, err
	}
	return CachedBlockMeta{
		Height:                  root.Height(),
		Hash:                    hash,
		ParentHash:              parentHash,
		StateRoot:               stateRoot,
		Miner:                   miner,
		Timestamp:               root.Timestamp(),
		Size:                    root.Size(),
		GasLimit:                root.GasLimit(),
		GasUsed:                 root.GasUsed(),
		EthTxCount:              root.EthTxCount(),
		TxCount:                 root.TxCount(),
		Bloom:                   bloom,
		TransactionsRoot:        transactionsRoot,
		BaseFee:                 baseFee,
		VirtualizedCosmosEvents: root.VirtualizedCosmosEvents(),
	}, nil
}

func mustMarshalBlockLogs(groups [][]*virtualbank.RPCLog) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootBlockLogs(seg)
	if err != nil {
		panic(err)
	}
	cgroups, err := root.NewGroups(int32(len(groups)))
	if err != nil {
		panic(err)
	}
	for i, logs := range groups {
		dst := cgroups.At(i)
		clogs, err := dst.NewLogs(int32(len(logs)))
		if err != nil {
			panic(err)
		}
		if err := setCapnpLogList(clogs, logs); err != nil {
			panic(err)
		}
	}
	return mustCapnpPayload(msg)
}

func unmarshalBlockLogsPayload(bz []byte) ([][]*virtualbank.RPCLog, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unmarshalJSON[[][]*virtualbank.RPCLog](bz)
	}
	root, err := kvcapnp.ReadRootBlockLogs(msg)
	if err != nil {
		return nil, err
	}
	if !root.HasGroups() {
		return [][]*virtualbank.RPCLog{}, nil
	}
	cgroups, err := root.Groups()
	if err != nil {
		return nil, err
	}
	out := make([][]*virtualbank.RPCLog, cgroups.Len())
	for i := 0; i < cgroups.Len(); i++ {
		group := cgroups.At(i)
		if !group.HasLogs() {
			out[i] = []*virtualbank.RPCLog{}
			continue
		}
		clogs, err := group.Logs()
		if err != nil {
			return nil, err
		}
		logs, err := capnpLogListToRPC(clogs)
		if err != nil {
			return nil, err
		}
		out[i] = logs
	}
	return out, nil
}

func unmarshalFilteredBlockLogsPayload(bz []byte, addresses []common.Address, topics [][]common.Hash) ([]*virtualbank.RPCLog, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return nil, err
	}
	if !ok {
		groups, err := unmarshalJSON[[][]*virtualbank.RPCLog](bz)
		if err != nil {
			return nil, err
		}
		out := make([]*virtualbank.RPCLog, 0)
		for _, group := range groups {
			for _, log := range group {
				if virtualbank.LogMatches(log, addresses, topics) {
					out = append(out, log)
				}
			}
		}
		return out, nil
	}
	root, err := kvcapnp.ReadRootBlockLogs(msg)
	if err != nil {
		return nil, err
	}
	if !root.HasGroups() {
		return []*virtualbank.RPCLog{}, nil
	}
	cgroups, err := root.Groups()
	if err != nil {
		return nil, err
	}
	out := make([]*virtualbank.RPCLog, 0)
	for i := 0; i < cgroups.Len(); i++ {
		group := cgroups.At(i)
		if !group.HasLogs() {
			continue
		}
		clogs, err := group.Logs()
		if err != nil {
			return nil, err
		}
		for j := 0; j < clogs.Len(); j++ {
			src := clogs.At(j)
			matched, err := capnpLogMatches(src, addresses, topics)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
			log, err := capnpLogToRPC(src)
			if err != nil {
				return nil, err
			}
			out = append(out, log)
		}
	}
	if out == nil {
		return []*virtualbank.RPCLog{}, nil
	}
	return out, nil
}

func mustMarshalReceipt(receipt CachedReceipt) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootReceipt(seg)
	if err != nil {
		panic(err)
	}
	root.SetStatus(receipt.Status)
	root.SetCumulativeGasUsed(receipt.CumulativeGasUsed)
	root.SetGasUsed(receipt.GasUsed)
	if receipt.Reason != nil {
		mustSet(root.SetReason(*receipt.Reason))
		root.SetReasonPresent(true)
	}
	if receipt.VMError != nil {
		mustSet(root.SetVmError(*receipt.VMError))
		root.SetVmErrorPresent(true)
	}
	mustSet(root.SetLogsBloom(common.FromHex(receipt.LogsBloom)))
	clogs, err := root.NewLogs(int32(len(receipt.Logs)))
	if err != nil {
		panic(err)
	}
	if err := setCapnpLogList(clogs, receipt.Logs); err != nil {
		panic(err)
	}
	mustSet(root.SetTransactionHash(common.HexToHash(receipt.TransactionHash).Bytes()))
	if receipt.ContractAddress != nil {
		mustSet(root.SetContractAddress(common.HexToAddress(*receipt.ContractAddress).Bytes()))
	}
	mustSet(root.SetBlockHash(common.HexToHash(receipt.BlockHash).Bytes()))
	root.SetBlockNumber(receipt.BlockNumber)
	root.SetTransactionIndex(receipt.TransactionIndex)
	if receipt.EffectiveGasPrice != "" {
		mustSet(setHexBigData(root.SetEffectiveGasPrice, receipt.EffectiveGasPrice))
		root.SetEffectiveGasPricePresent(true)
	}
	mustSet(root.SetFrom(common.HexToAddress(receipt.From).Bytes()))
	if receipt.To != nil {
		mustSet(root.SetTo(common.HexToAddress(*receipt.To).Bytes()))
	}
	root.SetType(receipt.Type)
	return mustCapnpPayload(msg)
}

func unmarshalReceiptPayload(bz []byte) (CachedReceipt, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return CachedReceipt{}, err
	}
	if !ok {
		return unmarshalJSON[CachedReceipt](bz)
	}
	root, err := kvcapnp.ReadRootReceipt(msg)
	if err != nil {
		return CachedReceipt{}, err
	}

	var receipt CachedReceipt
	receipt.Status = root.Status()
	receipt.CumulativeGasUsed = root.CumulativeGasUsed()
	receipt.GasUsed = root.GasUsed()
	if root.ReasonPresent() {
		reason, err := root.Reason()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.Reason = &reason
	}
	if root.VmErrorPresent() {
		vmErr, err := root.VmError()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.VMError = &vmErr
	}
	if root.HasLogsBloom() {
		logsBloom, err := root.LogsBloom()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.LogsBloom = hexutil.Encode(logsBloom)
	}
	if root.HasLogs() {
		clogs, err := root.Logs()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.Logs, err = capnpLogListToRPC(clogs)
		if err != nil {
			return CachedReceipt{}, err
		}
	}
	if root.HasTransactionHash() {
		txHash, err := root.TransactionHash()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.TransactionHash = common.BytesToHash(txHash).Hex()
	}
	if root.HasContractAddress() {
		contractAddress, err := root.ContractAddress()
		if err != nil {
			return CachedReceipt{}, err
		}
		v := common.BytesToAddress(contractAddress).Hex()
		receipt.ContractAddress = &v
	}
	if root.HasBlockHash() {
		blockHash, err := root.BlockHash()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.BlockHash = common.BytesToHash(blockHash).Hex()
	}
	receipt.BlockNumber = root.BlockNumber()
	receipt.TransactionIndex = root.TransactionIndex()
	if root.EffectiveGasPricePresent() || root.HasEffectiveGasPrice() {
		data, err := root.EffectiveGasPrice()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.EffectiveGasPrice = hexBigDataString(data)
	}
	if root.HasFrom() {
		from, err := root.From()
		if err != nil {
			return CachedReceipt{}, err
		}
		receipt.From = common.BytesToAddress(from).Hex()
	}
	if root.HasTo() {
		to, err := root.To()
		if err != nil {
			return CachedReceipt{}, err
		}
		v := common.BytesToAddress(to).Hex()
		receipt.To = &v
	}
	receipt.Type = root.Type()
	return receipt, nil
}

func mustMarshalRPCTransaction(tx *rpctypes.RPCTransaction) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootRPCTransaction(seg)
	if err != nil {
		panic(err)
	}
	if tx.BlockHash != nil {
		mustSet(root.SetBlockHash(tx.BlockHash.Bytes()))
	}
	if tx.BlockNumber != nil {
		mustSet(root.SetBlockNumber(hexutilBigBytes(tx.BlockNumber)))
		root.SetBlockNumberPresent(true)
	}
	mustSet(root.SetFrom(tx.From.Bytes()))
	root.SetGas(uint64(tx.Gas))
	if tx.GasPrice != nil {
		mustSet(root.SetGasPrice(hexutilBigBytes(tx.GasPrice)))
		root.SetGasPricePresent(true)
	}
	if tx.GasFeeCap != nil {
		mustSet(root.SetGasFeeCap(hexutilBigBytes(tx.GasFeeCap)))
		root.SetGasFeeCapPresent(true)
	}
	if tx.GasTipCap != nil {
		mustSet(root.SetGasTipCap(hexutilBigBytes(tx.GasTipCap)))
		root.SetGasTipCapPresent(true)
	}
	mustSet(root.SetHash(tx.Hash.Bytes()))
	mustSet(root.SetInput([]byte(tx.Input)))
	root.SetNonce(uint64(tx.Nonce))
	if tx.To != nil {
		mustSet(root.SetTo(tx.To.Bytes()))
	}
	if tx.TransactionIndex != nil {
		root.SetTransactionIndex(uint64(*tx.TransactionIndex))
		root.SetTransactionIndexPresent(true)
	}
	if tx.Value != nil {
		mustSet(root.SetValue(hexutilBigBytes(tx.Value)))
		root.SetValuePresent(true)
	}
	root.SetType(uint64(tx.Type))
	if tx.Accesses != nil {
		root.SetAccessesPresent(true)
		accesses := *tx.Accesses
		caccesses, err := root.NewAccesses(int32(len(accesses)))
		if err != nil {
			panic(err)
		}
		for i, access := range accesses {
			dst := caccesses.At(i)
			mustSet(dst.SetAddress(access.Address.Bytes()))
			keys, err := dst.NewStorageKeys(int32(len(access.StorageKeys)))
			if err != nil {
				panic(err)
			}
			for j, key := range access.StorageKeys {
				mustSet(keys.Set(j, key.Bytes()))
			}
		}
	}
	if tx.ChainID != nil {
		mustSet(root.SetChainId(hexutilBigBytes(tx.ChainID)))
		root.SetChainIdPresent(true)
	}
	if tx.V != nil {
		mustSet(root.SetV(hexutilBigBytes(tx.V)))
		root.SetVPresent(true)
	}
	if tx.R != nil {
		mustSet(root.SetR(hexutilBigBytes(tx.R)))
		root.SetRPresent(true)
	}
	if tx.S != nil {
		mustSet(root.SetS(hexutilBigBytes(tx.S)))
		root.SetSPresent(true)
	}
	root.SetVirtual(tx.Virtual)
	if tx.CosmosHash != nil {
		mustSet(root.SetCosmosHash(tx.CosmosHash.Bytes()))
	}
	return mustCapnpPayload(msg)
}

func unmarshalRPCTransactionPayload(bz []byte) (*rpctypes.RPCTransaction, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return nil, err
	}
	if !ok {
		tx, err := unmarshalJSON[rpctypes.RPCTransaction](bz)
		if err != nil {
			return nil, err
		}
		return &tx, nil
	}
	root, err := kvcapnp.ReadRootRPCTransaction(msg)
	if err != nil {
		return nil, err
	}
	var tx rpctypes.RPCTransaction
	if root.HasBlockHash() {
		v, err := root.BlockHash()
		if err != nil {
			return nil, err
		}
		h := common.BytesToHash(v)
		tx.BlockHash = &h
	}
	if root.BlockNumberPresent() || root.HasBlockNumber() {
		v, err := root.BlockNumber()
		if err != nil {
			return nil, err
		}
		tx.BlockNumber = hexutilBigFromData(v)
	}
	if root.HasFrom() {
		v, err := root.From()
		if err != nil {
			return nil, err
		}
		tx.From = common.BytesToAddress(v)
	}
	tx.Gas = hexutil.Uint64(root.Gas())
	if root.GasPricePresent() || root.HasGasPrice() {
		v, err := root.GasPrice()
		if err != nil {
			return nil, err
		}
		tx.GasPrice = hexutilBigFromData(v)
	}
	if root.GasFeeCapPresent() || root.HasGasFeeCap() {
		v, err := root.GasFeeCap()
		if err != nil {
			return nil, err
		}
		tx.GasFeeCap = hexutilBigFromData(v)
	}
	if root.GasTipCapPresent() || root.HasGasTipCap() {
		v, err := root.GasTipCap()
		if err != nil {
			return nil, err
		}
		tx.GasTipCap = hexutilBigFromData(v)
	}
	if root.HasHash() {
		v, err := root.Hash()
		if err != nil {
			return nil, err
		}
		tx.Hash = common.BytesToHash(v)
	}
	if root.HasInput() {
		v, err := root.Input()
		if err != nil {
			return nil, err
		}
		tx.Input = hexutil.Bytes(v)
	}
	tx.Nonce = hexutil.Uint64(root.Nonce())
	if root.HasTo() {
		v, err := root.To()
		if err != nil {
			return nil, err
		}
		addr := common.BytesToAddress(v)
		tx.To = &addr
	}
	if root.TransactionIndexPresent() {
		v := hexutil.Uint64(root.TransactionIndex())
		tx.TransactionIndex = &v
	}
	if root.ValuePresent() || root.HasValue() {
		v, err := root.Value()
		if err != nil {
			return nil, err
		}
		tx.Value = hexutilBigFromData(v)
	}
	tx.Type = hexutil.Uint64(root.Type())
	if root.AccessesPresent() || root.HasAccesses() {
		caccesses, err := root.Accesses()
		if err != nil {
			return nil, err
		}
		accesses := make(ethtypes.AccessList, caccesses.Len())
		for i := 0; i < caccesses.Len(); i++ {
			src := caccesses.At(i)
			if src.HasAddress() {
				v, err := src.Address()
				if err != nil {
					return nil, err
				}
				accesses[i].Address = common.BytesToAddress(v)
			}
			if src.HasStorageKeys() {
				keys, err := src.StorageKeys()
				if err != nil {
					return nil, err
				}
				accesses[i].StorageKeys = make([]common.Hash, keys.Len())
				for j := 0; j < keys.Len(); j++ {
					v, err := keys.At(j)
					if err != nil {
						return nil, err
					}
					accesses[i].StorageKeys[j] = common.BytesToHash(v)
				}
			}
		}
		tx.Accesses = &accesses
	}
	if root.ChainIdPresent() || root.HasChainId() {
		v, err := root.ChainId()
		if err != nil {
			return nil, err
		}
		tx.ChainID = hexutilBigFromData(v)
	}
	if root.VPresent() || root.HasV() {
		v, err := root.V()
		if err != nil {
			return nil, err
		}
		tx.V = hexutilBigFromData(v)
	}
	if root.RPresent() || root.HasR() {
		v, err := root.R()
		if err != nil {
			return nil, err
		}
		tx.R = hexutilBigFromData(v)
	}
	if root.SPresent() || root.HasS() {
		v, err := root.S()
		if err != nil {
			return nil, err
		}
		tx.S = hexutilBigFromData(v)
	}
	tx.Virtual = root.Virtual()
	if root.HasCosmosHash() {
		v, err := root.CosmosHash()
		if err != nil {
			return nil, err
		}
		h := common.BytesToHash(v)
		tx.CosmosHash = &h
	}
	return &tx, nil
}

func mustMarshalTxResult(tx *chaintypes.TxResult) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootTxResult(seg)
	if err != nil {
		panic(err)
	}
	if tx != nil {
		root.SetHeight(tx.Height)
		root.SetTxIndex(tx.TxIndex)
		root.SetMsgIndex(tx.MsgIndex)
		root.SetEthTxIndex(tx.EthTxIndex)
		root.SetFailed(tx.Failed)
		root.SetGasUsed(tx.GasUsed)
		root.SetCumulativeGasUsed(tx.CumulativeGasUsed)
	}
	return mustCapnpPayload(msg)
}

func unmarshalTxResultPayload(codec sdkcodec.Codec, bz []byte) (*chaintypes.TxResult, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return nil, err
	}
	if !ok {
		var tx chaintypes.TxResult
		if err := codec.Unmarshal(bz, &tx); err != nil {
			return nil, err
		}
		return &tx, nil
	}
	root, err := kvcapnp.ReadRootTxResult(msg)
	if err != nil {
		return nil, err
	}
	return &chaintypes.TxResult{
		Height:            root.Height(),
		TxIndex:           root.TxIndex(),
		MsgIndex:          root.MsgIndex(),
		EthTxIndex:        root.EthTxIndex(),
		Failed:            root.Failed(),
		GasUsed:           root.GasUsed(),
		CumulativeGasUsed: root.CumulativeGasUsed(),
	}, nil
}

func mustMarshalTracePayload(raw []byte) []byte {
	msg, seg := newKVCapnpMessage()
	root, err := kvcapnp.NewRootTracePayload(seg)
	if err != nil {
		panic(err)
	}
	mustSet(root.SetRaw(raw))
	return mustCapnpPayload(msg)
}

func unmarshalTracePayload(bz []byte) ([]byte, error) {
	msg, ok, err := capnpPayloadMessage(bz)
	if err != nil {
		return nil, err
	}
	if !ok {
		return append([]byte(nil), bz...), nil
	}
	root, err := kvcapnp.ReadRootTracePayload(msg)
	if err != nil {
		return nil, err
	}
	raw, err := root.Raw()
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), raw...), nil
}

func setCapnpLogList(dst kvcapnp.Log_List, logs []*virtualbank.RPCLog) error {
	for i, log := range logs {
		if log == nil {
			continue
		}
		if err := setCapnpLog(dst.At(i), log); err != nil {
			return err
		}
	}
	return nil
}

func setCapnpLog(dst kvcapnp.Log, log *virtualbank.RPCLog) error {
	if err := dst.SetAddress(log.Address.Bytes()); err != nil {
		return err
	}
	topics, err := dst.NewTopics(int32(len(log.Topics)))
	if err != nil {
		return err
	}
	for i, topic := range log.Topics {
		if err := topics.Set(i, topic.Bytes()); err != nil {
			return err
		}
	}
	if err := dst.SetData(log.Data); err != nil {
		return err
	}
	dst.SetBlockNumber(uint64(log.BlockNumber))
	if err := dst.SetTxHash(log.TxHash.Bytes()); err != nil {
		return err
	}
	dst.SetTxIndex(uint64(log.TxIndex))
	if err := dst.SetBlockHash(log.BlockHash.Bytes()); err != nil {
		return err
	}
	dst.SetIndex(uint64(log.Index))
	dst.SetRemoved(log.Removed)
	dst.SetVirtual(log.Virtual)
	if log.CosmosHash != nil {
		if err := dst.SetCosmosHash(log.CosmosHash.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func capnpLogListToRPC(src kvcapnp.Log_List) ([]*virtualbank.RPCLog, error) {
	out := make([]*virtualbank.RPCLog, src.Len())
	for i := 0; i < src.Len(); i++ {
		log, err := capnpLogToRPC(src.At(i))
		if err != nil {
			return nil, err
		}
		out[i] = log
	}
	return out, nil
}

func capnpLogToRPC(src kvcapnp.Log) (*virtualbank.RPCLog, error) {
	var out virtualbank.RPCLog
	if src.HasAddress() {
		bz, err := src.Address()
		if err != nil {
			return nil, err
		}
		out.Address = common.BytesToAddress(bz)
	}
	if src.HasTopics() {
		topics, err := src.Topics()
		if err != nil {
			return nil, err
		}
		out.Topics = make([]common.Hash, topics.Len())
		for i := 0; i < topics.Len(); i++ {
			bz, err := topics.At(i)
			if err != nil {
				return nil, err
			}
			out.Topics[i] = common.BytesToHash(bz)
		}
	}
	if src.HasData() {
		data, err := src.Data()
		if err != nil {
			return nil, err
		}
		out.Data = hexutil.Bytes(data)
	}
	out.BlockNumber = hexutil.Uint64(src.BlockNumber())
	if src.HasTxHash() {
		bz, err := src.TxHash()
		if err != nil {
			return nil, err
		}
		out.TxHash = common.BytesToHash(bz)
	}
	out.TxIndex = hexutil.Uint(src.TxIndex())
	if src.HasBlockHash() {
		bz, err := src.BlockHash()
		if err != nil {
			return nil, err
		}
		out.BlockHash = common.BytesToHash(bz)
	}
	out.Index = hexutil.Uint(src.Index())
	out.Removed = src.Removed()
	out.Virtual = src.Virtual()
	if src.HasCosmosHash() {
		bz, err := src.CosmosHash()
		if err != nil {
			return nil, err
		}
		h := common.BytesToHash(bz)
		out.CosmosHash = &h
	}
	return &out, nil
}

func capnpLogMatches(src kvcapnp.Log, addresses []common.Address, topics [][]common.Hash) (bool, error) {
	if len(addresses) > 0 {
		if !src.HasAddress() {
			return false, nil
		}
		bz, err := src.Address()
		if err != nil {
			return false, err
		}
		address := common.BytesToAddress(bz)
		matched := false
		for _, candidate := range addresses {
			if address == candidate {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	if len(topics) == 0 {
		return true, nil
	}
	if !src.HasTopics() {
		return false, nil
	}
	ctopics, err := src.Topics()
	if err != nil {
		return false, err
	}
	if len(topics) > ctopics.Len() {
		return false, nil
	}
	for i, sub := range topics {
		if len(sub) == 0 {
			continue
		}
		bz, err := ctopics.At(i)
		if err != nil {
			return false, err
		}
		topic := common.BytesToHash(bz)
		matched := false
		for _, candidate := range sub {
			if topic == candidate {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func hexutilBigBytes(v *hexutil.Big) []byte {
	if v == nil {
		return nil
	}
	return (*big.Int)(v).Bytes()
}

func hexutilBigFromData(bz []byte) *hexutil.Big {
	v := new(big.Int).SetBytes(bz)
	return (*hexutil.Big)(v)
}

func hexBigDataString(bz []byte) string {
	return hexutil.EncodeBig(new(big.Int).SetBytes(bz))
}

func setHexBigData(set func([]byte) error, raw string) error {
	v, err := hexutil.DecodeBig(raw)
	if err != nil {
		return fmt.Errorf("decode hex big %q: %w", raw, err)
	}
	return set(v.Bytes())
}

func mustSet(err error) {
	if err != nil {
		panic(err)
	}
}
