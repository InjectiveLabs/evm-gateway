package indexer

import (
	abci "github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	rpctypes "github.com/InjectiveLabs/web3-gateway/internal/evm/rpc/types"
)

// TxIndexer captures the indexing methods required by the RPC/backend layers.
type TxIndexer interface {
	IndexBlock(block *cmtypes.Block, txResults []*abci.ExecTxResult) error
	LastIndexedBlock() (int64, error)
	FirstIndexedBlock() (int64, error)
	GetByTxHash(hash common.Hash) (*chaintypes.TxResult, error)
	GetByBlockAndIndex(blockNumber int64, txIndex int32) (*chaintypes.TxResult, error)

	GetRPCTransactionByHash(hash common.Hash) (*rpctypes.RPCTransaction, error)
	GetRPCTransactionByBlockAndIndex(blockNumber int64, txIndex int32) (*rpctypes.RPCTransaction, error)
	GetReceiptByTxHash(hash common.Hash) (map[string]interface{}, error)
	GetBlockMetaByHeight(height int64) (*CachedBlockMeta, error)
	GetLogsByBlockHeight(height int64) ([][]*ethtypes.Log, error)
	GetLogsByBlockHash(hash common.Hash) ([][]*ethtypes.Log, error)
	IsBlockIndexed(height int64) (bool, error)
}
