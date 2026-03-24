package backend

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	sdkmath "cosmossdk.io/math"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"log/slog"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	appconfig "github.com/InjectiveLabs/web3-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/web3-gateway/internal/evm/rpc/types"
	txindexer "github.com/InjectiveLabs/web3-gateway/internal/indexer"
	"github.com/InjectiveLabs/web3-gateway/internal/syncstatus"
)

// BackendI implements the Cosmos and EVM backend.
type BackendI interface {
	EVMBackend
}

// EVMBackend implements the functionality shared within ethereum namespaces
// as defined by EIP-1474: https://github.com/ethereum/EIPs/blob/master/EIPS/eip-1474.md
// Implemented by Backend.
type EVMBackend interface {
	// Node specific queries
	Syncing() (interface{}, error)
	SetEtherbase(etherbase common.Address) bool
	SetGasPrice(gasPrice hexutil.Big) bool
	UnprotectedAllowed() bool
	RPCGasCap() uint64            // global gas cap for eth_call over rpc: DoS protection
	RPCEVMTimeout() time.Duration // global timeout for eth_call over rpc: DoS protection
	RPCTxFeeCap() float64         // RPCTxFeeCap is the global transaction fee(price * gaslimit) cap for send-transaction variants.
	RPCMinGasPrice() *big.Int

	// Sign Tx
	Sign(address common.Address, data hexutil.Bytes) (hexutil.Bytes, error)
	SendTransaction(args rpctypes.TransactionArgs) (common.Hash, error)
	SignTypedData(address common.Address, typedData apitypes.TypedData) (hexutil.Bytes, error)

	// Blocks Info
	BlockNumber() (hexutil.Uint64, error)
	GetBlockByNumber(blockNum rpctypes.BlockNumber, fullTx bool) (map[string]interface{}, error)
	GetBlockByHash(hash common.Hash, fullTx bool) (map[string]interface{}, error)
	GetBlockTransactionCountByHash(hash common.Hash) *hexutil.Uint
	GetBlockTransactionCountByNumber(blockNum rpctypes.BlockNumber) *hexutil.Uint
	TendermintBlockByNumber(blockNum rpctypes.BlockNumber) (*cmrpctypes.ResultBlock, error)
	TendermintBlockResultByNumber(height *int64) (*cmrpctypes.ResultBlockResults, error)
	TendermintBlockByHash(blockHash common.Hash) (*cmrpctypes.ResultBlock, error)
	BlockNumberFromTendermint(blockNrOrHash rpctypes.BlockNumberOrHash) (rpctypes.BlockNumber, error)
	BlockNumberFromTendermintByHash(blockHash common.Hash) (*big.Int, error)
	EthMsgsFromTendermintBlock(block *cmrpctypes.ResultBlock) []*evmtypes.MsgEthereumTx
	BlockBloom(blockRes *cmrpctypes.ResultBlockResults) (ethtypes.Bloom, error)
	HeaderByNumber(blockNum rpctypes.BlockNumber) (*ethtypes.Header, error)
	HeaderByHash(blockHash common.Hash) (*ethtypes.Header, error)
	RPCBlockFromTendermintBlock(
		resBlock *cmrpctypes.ResultBlock,
		blockRes *cmrpctypes.ResultBlockResults,
		fullTx bool,
	) (map[string]interface{}, error)
	EthBlockByNumber(blockNum rpctypes.BlockNumber) (*ethtypes.Block, error)
	EthBlockFromTendermintBlock(resBlock *cmrpctypes.ResultBlock, blockRes *cmrpctypes.ResultBlockResults) (*ethtypes.Block, error)

	// Account Info
	GetCode(address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error)
	GetBalance(address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (*hexutil.Big, error)
	GetStorageAt(address common.Address, key string, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error)
	GetProof(address common.Address, storageKeys []string, blockNrOrHash rpctypes.BlockNumberOrHash) (*rpctypes.AccountResult, error)
	GetTransactionCount(address common.Address, blockNum rpctypes.BlockNumber) (*hexutil.Uint64, error)

	// Chain Info
	ChainID() *hexutil.Big
	ChainConfig() *params.ChainConfig
	GlobalMinGasPrice() (sdkmath.LegacyDec, error)
	BaseFee(blockRes *cmrpctypes.ResultBlockResults) (*big.Int, error)
	CurrentHeader() (*ethtypes.Header, error)
	PendingTransactions() ([]sdk.Tx, error)
	GetCoinbase() (sdk.AccAddress, error)
	FeeHistory(blockCount math.HexOrDecimal64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*rpctypes.FeeHistoryResult, error)
	SuggestGasTipCap(baseFee *big.Int) (*big.Int, error)

	// Tx Info
	GetTransactionByHash(txHash common.Hash) (*rpctypes.RPCTransaction, error)
	GetTxByEthHash(txHash common.Hash) (*chaintypes.TxResult, error)
	GetTxByTxIndex(height int64, txIndex uint) (*chaintypes.TxResult, error)
	GetTransactionByBlockAndIndex(block *cmrpctypes.ResultBlock, idx hexutil.Uint) (*rpctypes.RPCTransaction, error)
	GetTransactionReceipt(hash common.Hash) (map[string]interface{}, error)
	GetTransactionByBlockHashAndIndex(hash common.Hash, idx hexutil.Uint) (*rpctypes.RPCTransaction, error)
	GetTransactionByBlockNumberAndIndex(blockNum rpctypes.BlockNumber, idx hexutil.Uint) (*rpctypes.RPCTransaction, error)
	GetTxHashByEthHash(common.Hash) (common.Hash, error)

	// Send Transaction
	Resend(args rpctypes.TransactionArgs, gasPrice *hexutil.Big, gasLimit *hexutil.Uint64) (common.Hash, error)
	SendRawTransaction(data hexutil.Bytes) (common.Hash, error)
	SetTxDefaults(args rpctypes.TransactionArgs) (rpctypes.TransactionArgs, error)
	EstimateGas(args rpctypes.TransactionArgs, blockNrOptional *rpctypes.BlockNumber) (hexutil.Uint64, error)
	DoCall(args rpctypes.TransactionArgs, blockNr rpctypes.BlockNumber, overrides *json.RawMessage) (*evmtypes.MsgEthereumTxResponse, error)
	GasPrice() (*hexutil.Big, error)

	// Filter API
	GetLogs(hash common.Hash) ([][]*ethtypes.Log, error)
	GetLogsByHeight(height *int64) ([][]*ethtypes.Log, error)
	GetBlockBloomByHeight(height int64) (ethtypes.Bloom, error)
	BloomStatus() (uint64, uint64)

	// Tracing
	TraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (interface{}, error)
	TraceBlock(height rpctypes.BlockNumber, config *rpctypes.TraceConfig, block *cmrpctypes.ResultBlock) ([]*rpctypes.TxTraceResult, error)
	TraceCall(args rpctypes.TransactionArgs, blockNr rpctypes.BlockNumberOrHash, config *rpctypes.TraceConfig) (interface{}, error)
}

var _ BackendI = (*Backend)(nil)

var bAttributeKeyEthereumBloom = []byte(evmtypes.AttributeKeyEthereumBloom)

type ProcessBlocker func(
	tendermintBlock *cmrpctypes.ResultBlock,
	ethBlock map[string]interface{},
	rewardPercentiles []float64,
	tendermintBlockResult *cmrpctypes.ResultBlockResults,
	targetOneFeeHistory *rpctypes.OneFeeHistory,
) error

// Backend implements the BackendI interface
type Backend struct {
	ctx                 context.Context
	clientCtx           client.Context
	queryClient         *rpctypes.QueryClient // gRPC query client
	logger              *slog.Logger
	cfg                 appconfig.Config
	allowUnprotectedTxs bool
	indexer             txindexer.TxIndexer
	syncStatus          *syncstatus.Tracker
	processBlocker      ProcessBlocker
}

// NewBackend creates a new Backend instance for cosmos and ethereum namespaces
func NewBackend(
	logger *slog.Logger,
	cfg appconfig.Config,
	clientCtx client.Context,
	allowUnprotectedTxs bool,
	indexer txindexer.TxIndexer,
	syncStatus *syncstatus.Tracker,
) *Backend {
	b := &Backend{
		ctx:                 context.Background(),
		clientCtx:           clientCtx,
		queryClient:         rpctypes.NewQueryClient(clientCtx),
		logger:              logger.With("module", "backend"),
		cfg:                 cfg,
		allowUnprotectedTxs: allowUnprotectedTxs,
		indexer:             indexer,
		syncStatus:          syncStatus,
	}
	b.processBlocker = b.processBlock
	return b
}
