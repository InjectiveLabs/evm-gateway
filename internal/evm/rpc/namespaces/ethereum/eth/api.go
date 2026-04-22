package eth

import (
	"context"
	"encoding/json"

	"github.com/ethereum/go-ethereum/rpc"

	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"upd.dev/xlab/gotracer"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// The Ethereum API allows applications to connect to an Injective node that is
// part of the Injective blockchain. Developers can interact with on-chain EVM data
// and send different types of transactions to the network by utilizing the
// endpoints provided by the API. The API follows a JSON-RPC standard. If not
// otherwise specified, the interface is derived from the Alchemy Ethereum API:
// https://docs.alchemy.com/alchemy/apis/ethereum
type EthereumAPI interface {
	// Getting Blocks
	//
	// Retrieves information from a particular block in the blockchain.
	BlockNumber(ctx context.Context) (hexutil.Uint64, error)
	GetBlockByNumber(ctx context.Context, ethBlockNum rpctypes.BlockNumber, fullTx bool) (map[string]interface{}, error)
	GetBlockByHash(ctx context.Context, hash common.Hash, fullTx bool) (map[string]interface{}, error)
	GetBlockTransactionCountByHash(ctx context.Context, hash common.Hash) *hexutil.Uint
	GetBlockTransactionCountByNumber(ctx context.Context, blockNum rpctypes.BlockNumber) *hexutil.Uint

	// Reading Transactions
	//
	// Retrieves information on the state data for addresses regardless of whether
	// it is a user or a smart contract.
	GetTransactionByHash(ctx context.Context, hash common.Hash) (*rpctypes.RPCTransaction, error)
	GetTransactionCount(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (*hexutil.Uint64, error)
	GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]interface{}, error)
	GetTransactionByBlockHashAndIndex(ctx context.Context, hash common.Hash, idx hexutil.Uint) (*rpctypes.RPCTransaction, error)
	GetTransactionByBlockNumberAndIndex(ctx context.Context, blockNum rpctypes.BlockNumber, idx hexutil.Uint) (*rpctypes.RPCTransaction, error)
	GetBlockReceipts(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) ([]map[string]interface{}, error)

	// Writing Transactions
	//
	// Allows developers to both send ETH from one address to another, write data
	// on-chain, and interact with smart contracts.
	SendRawTransaction(ctx context.Context, data hexutil.Bytes) (common.Hash, error)
	// eth_sendPrivateTransaction
	// eth_cancel	PrivateTransaction

	// Account Information
	//
	// Returns information regarding an address's stored on-chain data.
	GetBalance(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (*hexutil.Big, error)
	GetStorageAt(ctx context.Context, address common.Address, key string, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error)
	GetCode(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error)
	GetProof(ctx context.Context, address common.Address, storageKeys []string, blockNrOrHash rpctypes.BlockNumberOrHash) (*rpctypes.AccountResult, error)

	// EVM/Smart Contract Execution
	//
	// Allows developers to read data from the blockchain which includes executing
	// smart contracts. However, no data is published to the Ethereum network.
	Call(ctx context.Context, args rpctypes.TransactionArgs, blockNrOrHash rpctypes.BlockNumberOrHash, overrides *json.RawMessage) (hexutil.Bytes, error)

	// Chain Information
	//
	// Returns information on the Ethereum network and internal settings.
	ProtocolVersion() hexutil.Uint
	GasPrice(ctx context.Context) (*hexutil.Big, error)
	EstimateGas(ctx context.Context, args rpctypes.TransactionArgs, blockNrOptional *rpctypes.BlockNumber) (hexutil.Uint64, error)
	FeeHistory(ctx context.Context, blockCount math.HexOrDecimal64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*rpctypes.FeeHistoryResult, error)
	MaxPriorityFeePerGas(ctx context.Context) (*hexutil.Big, error)
	ChainId() (*hexutil.Big, error)

	// Getting Uncles
	//
	// Returns information on uncle blocks are which are network rejected blocks and replaced by a canonical block instead.
	GetUncleByBlockHashAndIndex(hash common.Hash, idx hexutil.Uint) map[string]interface{}
	GetUncleByBlockNumberAndIndex(number, idx hexutil.Uint) map[string]interface{}
	GetUncleCountByBlockHash(hash common.Hash) hexutil.Uint
	GetUncleCountByBlockNumber(blockNum rpctypes.BlockNumber) hexutil.Uint

	// Proof of Work
	Hashrate() hexutil.Uint64
	Mining() bool

	// Other
	Syncing(ctx context.Context) (interface{}, error)
	Coinbase(ctx context.Context) (string, error)
	GetTransactionLogs(ctx context.Context, txHash common.Hash) ([]*virtualbank.RPCLog, error)
	FillTransaction(ctx context.Context, args rpctypes.TransactionArgs) (*rpctypes.SignTransactionResult, error)
	GetPendingTransactions(ctx context.Context) ([]*rpctypes.RPCTransaction, error)
	// eth_signTransaction (on Ethereum.org)
	// eth_getCompilers (on Ethereum.org)
	// eth_compileSolidity (on Ethereum.org)
	// eth_compileLLL (on Ethereum.org)
	// eth_compileSerpent (on Ethereum.org)
	// eth_getWork (on Ethereum.org)
	// eth_submitWork (on Ethereum.org)
	// eth_submitHashrate (on Ethereum.org)
}

var _ EthereumAPI = (*PublicAPI)(nil)

// PublicAPI is the eth_ prefixed set of APIs in the Web3 JSON-RPC spec.
type PublicAPI struct {
	ctx     context.Context
	logger  *slog.Logger
	backend backend.EVMBackend
}

// NewPublicAPI creates an instance of the public ETH Web3 API.
func NewPublicAPI(logger *slog.Logger, evmBackend backend.EVMBackend) *PublicAPI {
	api := &PublicAPI{
		ctx:     context.Background(),
		logger:  logger.With("client", "json-rpc"),
		backend: evmBackend,
	}

	return api
}

///////////////////////////////////////////////////////////////////////////////
///                           Blocks						                            ///
///////////////////////////////////////////////////////////////////////////////

// BlockNumber returns the current block number.
func (e *PublicAPI) BlockNumber(ctx context.Context) (hexutil.Uint64, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_blockNumber")
	return e.backend.WithContext(ctx).BlockNumber()
}

// GetBlockByNumber returns the block identified by number.
func (e *PublicAPI) GetBlockByNumber(ctx context.Context, ethBlockNum rpctypes.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBlockByNumber", "number", ethBlockNum, "full", fullTx)
	return e.backend.WithContext(ctx).GetBlockByNumber(ethBlockNum, fullTx)
}

// GetBlockByHash returns the block identified by hash.
func (e *PublicAPI) GetBlockByHash(ctx context.Context, hash common.Hash, fullTx bool) (map[string]interface{}, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBlockByHash", "hash", hash.Hex(), "full", fullTx)
	return e.backend.WithContext(ctx).GetBlockByHash(hash, fullTx)
}

///////////////////////////////////////////////////////////////////////////////
///                           Read Txs					                            ///
///////////////////////////////////////////////////////////////////////////////

// GetTransactionByHash returns the transaction identified by hash.
func (e *PublicAPI) GetTransactionByHash(ctx context.Context, hash common.Hash) (*rpctypes.RPCTransaction, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getTransactionByHash", "hash", hash.Hex())
	return e.backend.WithContext(ctx).GetTransactionByHash(hash)
}

// GetTransactionCount returns the number of transactions at the given address up to the given block number.
func (e *PublicAPI) GetTransactionCount(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (*hexutil.Uint64, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getTransactionCount", "address", address.Hex(), "block number or hash", blockNrOrHash)
	backend := e.backend.WithContext(ctx)
	blockNum, err := backend.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	return backend.GetTransactionCount(address, blockNum)
}

// GetTransactionReceipt returns the transaction receipt identified by hash.
func (e *PublicAPI) GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	defer gotracer.Trace(&ctx)()
	hexTx := hash.Hex()
	e.logger.Debug("eth_getTransactionReceipt", "hash", hexTx)
	return e.backend.WithContext(ctx).GetTransactionReceipt(hash)
}

// GetBlockReceipts returns all transaction receipts for the provided block.
func (e *PublicAPI) GetBlockReceipts(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) ([]map[string]interface{}, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBlockReceipts", "block number or hash", blockNrOrHash)
	return e.backend.WithContext(ctx).GetBlockReceipts(blockNrOrHash)
}

// GetBlockTransactionCountByHash returns the number of transactions in the block identified by hash.
func (e *PublicAPI) GetBlockTransactionCountByHash(ctx context.Context, hash common.Hash) *hexutil.Uint {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBlockTransactionCountByHash", "hash", hash.Hex())
	return e.backend.WithContext(ctx).GetBlockTransactionCountByHash(hash)
}

// GetBlockTransactionCountByNumber returns the number of transactions in the block identified by number.
func (e *PublicAPI) GetBlockTransactionCountByNumber(ctx context.Context, blockNum rpctypes.BlockNumber) *hexutil.Uint {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBlockTransactionCountByNumber", "height", blockNum.Int64())
	return e.backend.WithContext(ctx).GetBlockTransactionCountByNumber(blockNum)
}

// GetTransactionByBlockHashAndIndex returns the transaction identified by hash and index.
func (e *PublicAPI) GetTransactionByBlockHashAndIndex(ctx context.Context, hash common.Hash, idx hexutil.Uint) (*rpctypes.RPCTransaction, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getTransactionByBlockHashAndIndex", "hash", hash.Hex(), "index", idx)
	return e.backend.WithContext(ctx).GetTransactionByBlockHashAndIndex(hash, idx)
}

// GetTransactionByBlockNumberAndIndex returns the transaction identified by number and index.
func (e *PublicAPI) GetTransactionByBlockNumberAndIndex(ctx context.Context, blockNum rpctypes.BlockNumber, idx hexutil.Uint) (*rpctypes.RPCTransaction, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getTransactionByBlockNumberAndIndex", "number", blockNum, "index", idx)
	return e.backend.WithContext(ctx).GetTransactionByBlockNumberAndIndex(blockNum, idx)
}

///////////////////////////////////////////////////////////////////////////////
///                           Write Txs					                            ///
///////////////////////////////////////////////////////////////////////////////

// SendRawTransaction send a raw Ethereum transaction.
func (e *PublicAPI) SendRawTransaction(ctx context.Context, data hexutil.Bytes) (common.Hash, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_sendRawTransaction", "length", len(data))
	return e.backend.WithContext(ctx).SendRawTransaction(data)
}

///////////////////////////////////////////////////////////////////////////////
///                           Account Information				                    ///
///////////////////////////////////////////////////////////////////////////////

// GetBalance returns the provided account's balance up to the provided block number.
func (e *PublicAPI) GetBalance(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (*hexutil.Big, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getBalance", "address", address.String(), "block number or hash", blockNrOrHash)
	return e.backend.WithContext(ctx).GetBalance(address, blockNrOrHash)
}

// GetStorageAt returns the contract storage at the given address, block number, and key.
func (e *PublicAPI) GetStorageAt(ctx context.Context, address common.Address, key string, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getStorageAt", "address", address.Hex(), "key", key, "block number or hash", blockNrOrHash)
	return e.backend.WithContext(ctx).GetStorageAt(address, key, blockNrOrHash)
}

// GetCode returns the contract code at the given address and block number.
func (e *PublicAPI) GetCode(ctx context.Context, address common.Address, blockNrOrHash rpctypes.BlockNumberOrHash) (hexutil.Bytes, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getCode", "address", address.Hex(), "block number or hash", blockNrOrHash)
	return e.backend.WithContext(ctx).GetCode(address, blockNrOrHash)
}

// GetProof returns an account object with proof and any storage proofs
func (e *PublicAPI) GetProof(ctx context.Context, address common.Address,
	storageKeys []string,
	blockNrOrHash rpctypes.BlockNumberOrHash,
) (*rpctypes.AccountResult, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getProof", "address", address.Hex(), "keys", storageKeys, "block number or hash", blockNrOrHash)
	return e.backend.WithContext(ctx).GetProof(address, storageKeys, blockNrOrHash)
}

///////////////////////////////////////////////////////////////////////////////
///                           EVM/Smart Contract Execution				          ///
///////////////////////////////////////////////////////////////////////////////

// Call performs a raw contract call.
func (e *PublicAPI) Call(ctx context.Context, args rpctypes.TransactionArgs,
	blockNrOrHash rpctypes.BlockNumberOrHash,
	overrides *json.RawMessage,
) (hexutil.Bytes, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_call", "args", args.String(), "block number or hash", blockNrOrHash)

	backend := e.backend.WithContext(ctx)
	blockNum, err := backend.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	data, err := backend.DoCall(args, blockNum, overrides)
	if err != nil {
		return []byte{}, err
	}

	return hexutil.Bytes(data.Ret), nil
}

///////////////////////////////////////////////////////////////////////////////
///                           Event Logs													          ///
///////////////////////////////////////////////////////////////////////////////
// FILTER API at ./filters/api.go

///////////////////////////////////////////////////////////////////////////////
///                           Chain Information										          ///
///////////////////////////////////////////////////////////////////////////////

// ProtocolVersion returns the supported Ethereum protocol version.
func (e *PublicAPI) ProtocolVersion() hexutil.Uint {
	e.logger.Debug("eth_protocolVersion")
	return hexutil.Uint(65)
}

// GasPrice returns the current gas price based on Ethermint's gas price oracle.
func (e *PublicAPI) GasPrice(ctx context.Context) (*hexutil.Big, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_gasPrice")
	return e.backend.WithContext(ctx).GasPrice()
}

// EstimateGas returns an estimate of gas usage for the given smart contract call.
func (e *PublicAPI) EstimateGas(ctx context.Context, args rpctypes.TransactionArgs, blockNrOptional *rpctypes.BlockNumber) (hexutil.Uint64, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_estimateGas")
	return e.backend.WithContext(ctx).EstimateGas(args, blockNrOptional)
}

func (e *PublicAPI) FeeHistory(ctx context.Context, blockCount math.HexOrDecimal64,
	lastBlock rpc.BlockNumber,
	rewardPercentiles []float64,
) (*rpctypes.FeeHistoryResult, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_feeHistory")
	return e.backend.WithContext(ctx).FeeHistory(blockCount, lastBlock, rewardPercentiles)
}

// MaxPriorityFeePerGas returns a suggestion for a gas tip cap for dynamic fee transactions.
func (e *PublicAPI) MaxPriorityFeePerGas(ctx context.Context) (*hexutil.Big, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_maxPriorityFeePerGas")
	backend := e.backend.WithContext(ctx)
	head, err := backend.CurrentHeader()
	if err != nil {
		return nil, err
	}
	tipcap, err := backend.SuggestGasTipCap(head.BaseFee)
	if err != nil {
		return nil, err
	}
	return (*hexutil.Big)(tipcap), nil
}

// ChainId is the EIP-155 replay-protection chain id for the current ethereum chain config.
func (e *PublicAPI) ChainId() (*hexutil.Big, error) { //nolint
	e.logger.Debug("eth_chainId")
	return e.backend.ChainID(), nil
}

///////////////////////////////////////////////////////////////////////////////
///                           Uncles															          ///
///////////////////////////////////////////////////////////////////////////////

// GetUncleByBlockHashAndIndex returns the uncle identified by hash and index. Always returns nil.
func (e *PublicAPI) GetUncleByBlockHashAndIndex(_ common.Hash, _ hexutil.Uint) map[string]interface{} {
	return nil
}

// GetUncleByBlockNumberAndIndex returns the uncle identified by number and index. Always returns nil.
func (e *PublicAPI) GetUncleByBlockNumberAndIndex(_, _ hexutil.Uint) map[string]interface{} {
	return nil
}

// GetUncleCountByBlockHash returns the number of uncles in the block identified by hash. Always zero.
func (e *PublicAPI) GetUncleCountByBlockHash(_ common.Hash) hexutil.Uint {
	return 0
}

// GetUncleCountByBlockNumber returns the number of uncles in the block identified by number. Always zero.
func (e *PublicAPI) GetUncleCountByBlockNumber(_ rpctypes.BlockNumber) hexutil.Uint {
	return 0
}

///////////////////////////////////////////////////////////////////////////////
///                           Proof of Work												          ///
///////////////////////////////////////////////////////////////////////////////

// Hashrate returns the current node's hashrate. Always 0.
func (e *PublicAPI) Hashrate() hexutil.Uint64 {
	e.logger.Debug("eth_hashrate")
	return 0
}

// Mining returns whether or not this node is currently mining. Always false.
func (e *PublicAPI) Mining() bool {
	e.logger.Debug("eth_mining")
	return false
}

///////////////////////////////////////////////////////////////////////////////
///                           Other 															          ///
///////////////////////////////////////////////////////////////////////////////

// Syncing returns false in case the node is currently not syncing with the network. It can be up to date or has not
// yet received the latest block headers from its pears. In case it is synchronizing:
// - startingBlock: block number this node started to synchronize from
// - currentBlock:  block number this node is currently importing
// - highestBlock:  block number of the highest block header this node has received from peers
// - pulledStates:  number of state entries processed until now
// - knownStates:   number of known state entries that still need to be pulled
func (e *PublicAPI) Syncing(ctx context.Context) (interface{}, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_syncing")
	return e.backend.WithContext(ctx).Syncing()
}

// Coinbase is the address that staking rewards will be send to (alias for Etherbase).
func (e *PublicAPI) Coinbase(ctx context.Context) (string, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_coinbase")

	coinbase, err := e.backend.WithContext(ctx).GetCoinbase()
	if err != nil {
		return "", err
	}
	ethAddr := common.BytesToAddress(coinbase.Bytes())
	return ethAddr.Hex(), nil
}

// GetTransactionLogs returns the receipt logs for a transaction hash. The logs
// may come from an indexed receipt or a live receipt, and may include
// virtualized Cosmos x/bank events when that mode is enabled.
func (e *PublicAPI) GetTransactionLogs(ctx context.Context, txHash common.Hash) ([]*virtualbank.RPCLog, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getTransactionLogs", "hash", txHash)
	receipt, err := e.backend.WithContext(ctx).GetTransactionReceipt(txHash)
	if err != nil || receipt == nil {
		return nil, err
	}
	logsRaw, ok := receipt["logs"]
	if !ok || logsRaw == nil {
		return nil, nil
	}
	switch logs := logsRaw.(type) {
	case []*virtualbank.RPCLog:
		return logs, nil
	case []*ethtypes.Log:
		return virtualbank.WrapLogs(logs, false, nil), nil
	default:
		return nil, nil
	}
}

// FillTransaction fills the defaults (nonce, gas, gasPrice or 1559 fields)
// on a given unsigned transaction, and returns it to the caller for further
// processing (signing + broadcast).
func (e *PublicAPI) FillTransaction(ctx context.Context, args rpctypes.TransactionArgs) (*rpctypes.SignTransactionResult, error) {
	defer gotracer.Trace(&ctx)()
	// Set some sanity defaults and terminate on failure
	args, err := e.backend.WithContext(ctx).SetTxDefaults(args)
	if err != nil {
		return nil, err
	}

	// Assemble the transaction and obtain rlp
	tx := args.ToTransaction().AsTransaction()

	data, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return &rpctypes.SignTransactionResult{
		Raw: data,
		Tx:  tx,
	}, nil
}

// GetPendingTransactions returns the transactions that are in the transaction pool
// and have a from address that is one of the accounts this node manages.
func (e *PublicAPI) GetPendingTransactions(ctx context.Context) ([]*rpctypes.RPCTransaction, error) {
	defer gotracer.Trace(&ctx)()
	e.logger.Debug("eth_getPendingTransactions")

	backend := e.backend.WithContext(ctx)
	txs, err := backend.PendingTransactions()
	if err != nil {
		return nil, err
	}

	result := make([]*rpctypes.RPCTransaction, 0, len(txs))
	for _, tx := range txs {
		for _, msg := range tx.GetMsgs() {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				// not valid ethereum tx
				break
			}

			rpctx, err := rpctypes.NewTransactionFromMsg(
				ethMsg,
				common.Hash{},
				uint64(0),
				uint64(0),
				nil,
				backend.ChainID().ToInt(),
			)
			if err != nil {
				return nil, err
			}

			result = append(result, rpctx)
		}
	}

	return result, nil
}
