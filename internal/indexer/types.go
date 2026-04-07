package indexer

import (
	"context"
	"encoding/json"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

type BlockIndexStats struct {
	IndexedEthTxs int64
}

// TxIndexer captures the indexing methods required by the RPC/backend layers.
type TxIndexer interface {
	WithContext(ctx context.Context) TxIndexer

	IndexBlock(block *cmtypes.Block, txResults []*abci.ExecTxResult) error
	DeleteBlock(height int64) error
	LastIndexedBlock() (int64, error)
	FirstIndexedBlock() (int64, error)
	GetByTxHash(hash common.Hash) (*chaintypes.TxResult, error)
	GetByBlockAndIndex(blockNumber int64, txIndex int32) (*chaintypes.TxResult, error)

	GetRPCTransactionByHash(hash common.Hash) (*rpctypes.RPCTransaction, error)
	GetRPCTransactionByBlockAndIndex(blockNumber int64, txIndex int32) (*rpctypes.RPCTransaction, error)
	GetRPCTransactionHashesByBlockHeight(height int64) ([]common.Hash, error)
	GetReceiptByTxHash(hash common.Hash) (map[string]interface{}, error)
	GetBlockMetaByHeight(height int64) (*CachedBlockMeta, error)
	GetBlockMetaByHash(hash common.Hash) (*CachedBlockMeta, error)
	GetLogsByBlockHeight(height int64) ([][]*ethtypes.Log, error)
	GetLogsByBlockHash(hash common.Hash) ([][]*ethtypes.Log, error)
	SetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig, raw json.RawMessage) error
	GetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (json.RawMessage, error)
	SetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig, raw json.RawMessage) error
	GetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig) (json.RawMessage, error)
	IsBlockIndexed(height int64) (bool, error)
}
