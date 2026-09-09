package backend

import (
	"bytes"
	"fmt"
	"sort"

	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

type kvPairs struct {
	Pairs []kvPair `protobuf:"bytes,1,rep,name=pairs,proto3" json:"pairs"`
}

func (m *kvPairs) Reset()         { *m = kvPairs{} }
func (m *kvPairs) String() string { return proto.CompactTextString(m) }
func (*kvPairs) ProtoMessage()    {}

type kvPair struct {
	Key   []byte `protobuf:"bytes,1,opt,name=key,proto3" json:"key,omitempty"`
	Value []byte `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
}

func (m *kvPair) Reset()         { *m = kvPair{} }
func (m *kvPair) String() string { return proto.CompactTextString(m) }
func (*kvPair) ProtoMessage()    {}

type storageItem struct {
	key   common.Hash
	value common.Hash
}

// StorageRangeAt returns the storage at the given block height for a contract.
// The txIndex argument is accepted for RPC compatibility but ignored.
func (b *Backend) StorageRangeAt(
	blockNrOrHash rpctypes.BlockNumberOrHash,
	_ int,
	contractAddress common.Address,
	keyStart hexutil.Bytes,
	maxResult int,
) (rpctypes.StorageRangeResult, error) {
	b = b.WithContext(b.operationContext()).(*Backend)
	blockNum, err := b.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return rpctypes.StorageRangeResult{}, err
	}
	blockNum, err = b.ResolveEarliestBlockNumber(blockNum, earliestStorage)
	if err != nil {
		return rpctypes.StorageRangeResult{}, err
	}

	opts := cmrpcclient.ABCIQueryOptions{
		Height: blockNum.Int64(),
		Prove:  false,
	}
	path := fmt.Sprintf("/store/%s/subspace", evmtypes.StoreKey)
	prefix := evmtypes.AddressStoragePrefix(contractAddress)
	res, err := b.clientCtx.Client.ABCIQueryWithOptions(b.contextForCometHeight(opts.Height), path, cmbytes.HexBytes(prefix), opts)
	if err != nil {
		return rpctypes.StorageRangeResult{}, err
	}
	if res == nil || res.Response.Code != 0 {
		return rpctypes.StorageRangeResult{}, fmt.Errorf("storage history unavailable at height %d", blockNum.Int64())
	}

	result := rpctypes.StorageRangeResult{
		Storage: map[common.Hash]rpctypes.StorageRangeEntry{},
	}
	if len(res.Response.Value) == 0 || maxResult <= 0 {
		return result, nil
	}

	var pairs kvPairs
	if err := proto.Unmarshal(res.Response.Value, &pairs); err != nil {
		return rpctypes.StorageRangeResult{}, err
	}

	items := make([]storageItem, 0, len(pairs.Pairs))
	for _, pair := range pairs.Pairs {
		if len(pair.Key) < len(prefix) {
			continue
		}
		slotBytes := pair.Key[len(prefix):]
		items = append(items, storageItem{
			key:   common.BytesToHash(slotBytes),
			value: common.BytesToHash(pair.Value),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return bytes.Compare(items[i].key.Bytes(), items[j].key.Bytes()) < 0
	})

	startKey := common.BytesToHash(keyStart)
	filtered := items
	if len(keyStart) > 0 {
		filtered = items[:0]
		for _, item := range items {
			if bytes.Compare(item.key.Bytes(), startKey.Bytes()) >= 0 {
				filtered = append(filtered, item)
			}
		}
	}

	if len(filtered) > maxResult {
		next := filtered[maxResult].key
		result.NextKey = &next
		filtered = filtered[:maxResult]
	}

	for _, item := range filtered {
		key := item.key
		result.Storage[key] = rpctypes.StorageRangeEntry{
			Key:   &key,
			Value: item.value,
		}
	}

	return result, nil
}
