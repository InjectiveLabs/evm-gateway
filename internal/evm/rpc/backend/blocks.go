package backend

import (
	"bytes"
	"fmt"
	"math/big"
	"strconv"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"upd.dev/xlab/gotracer"

	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

// BlockNumber returns the current block number in abci app state. Because abci
// app state could lag behind from tendermint latest block, it's more stable for
// the client to use the latest block number in abci app state than tendermint
// rpc.
func (b *Backend) BlockNumber() (hexutil.Uint64, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if b.cfg.OfflineRPCOnly && b.indexer != nil {
		last, err := b.indexer.LastIndexedBlock()
		if err != nil {
			return hexutil.Uint64(0), err
		}
		if last < 0 {
			return hexutil.Uint64(0), nil
		}
		return hexutil.Uint64(last), nil
	}

	// do any grpc query, ignore the response and use the returned block height
	var header metadata.MD
	_, err := b.queryClient.Params(b.ctx, &evmtypes.QueryParamsRequest{}, grpc.Header(&header))
	if err != nil {
		return hexutil.Uint64(0), err
	}

	blockHeightHeader := header.Get(grpctypes.GRPCBlockHeightHeader)
	if headerLen := len(blockHeightHeader); headerLen != 1 {
		return 0, fmt.Errorf("unexpected '%s' gRPC header length; got %d, expected: %d", grpctypes.GRPCBlockHeightHeader, headerLen, 1)
	}

	height, err := strconv.ParseUint(blockHeightHeader[0], 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, "failed to parse block height")
	}

	return hexutil.Uint64(height), nil
}

// GetBlockByNumber returns the JSON-RPC compatible Ethereum block identified by
// block number. Depending on fullTx it either returns the full transaction
// objects or if false only the hashes of the transactions.
func (b *Backend) GetBlockByNumber(blockNum rpctypes.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	meta, err := b.cachedBlockMetaByNumber(blockNum)
	if err == nil && meta != nil {
		block, cacheErr := b.rpcBlockFromCachedMeta(meta, fullTx)
		if cacheErr == nil {
			return block, nil
		}
		if b.cfg.OfflineRPCOnly {
			return nil, cacheErr
		}
		b.logger.Debug("cached block-by-number reconstruction failed; falling back to live rpc", "height", meta.Height, "error", cacheErr.Error())
	} else if err != nil && !isIndexerCacheMiss(err) {
		if b.cfg.OfflineRPCOnly {
			return nil, err
		}
		b.logger.Debug("cached block-by-number lookup failed; falling back to live rpc", "height", blockNum, "error", err.Error())
	} else if b.cfg.OfflineRPCOnly {
		return nil, nil
	}

	resBlock, err := b.TendermintBlockByNumber(blockNum)
	if err != nil {
		return nil, nil
	}

	// return if requested block height is greater than the current one
	if resBlock == nil || resBlock.Block == nil {
		return nil, nil
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		b.logger.Debug("failed to fetch block result from Tendermint", "height", blockNum, "error", err.Error())
		return nil, nil
	}

	res, err := b.RPCBlockFromTendermintBlock(resBlock, blockRes, fullTx)
	if err != nil {
		b.logger.Debug("GetEthBlockFromTendermint failed", "height", blockNum, "error", err.Error())
		return nil, err
	}

	return res, nil
}

// GetBlockByHash returns the JSON-RPC compatible Ethereum block identified by
// hash.
func (b *Backend) GetBlockByHash(hash common.Hash, fullTx bool) (map[string]interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	meta, err := b.cachedBlockMetaByHash(hash)
	if err == nil && meta != nil {
		block, cacheErr := b.rpcBlockFromCachedMeta(meta, fullTx)
		if cacheErr == nil {
			return block, nil
		}
		if b.cfg.OfflineRPCOnly {
			return nil, cacheErr
		}
		b.logger.Debug("cached block-by-hash reconstruction failed; falling back to live rpc", "hash", hash.Hex(), "error", cacheErr.Error())
	} else if err != nil && !isIndexerCacheMiss(err) {
		if b.cfg.OfflineRPCOnly {
			return nil, err
		}
		b.logger.Debug("cached block-by-hash lookup failed; falling back to live rpc", "hash", hash.Hex(), "error", err.Error())
	} else if b.cfg.OfflineRPCOnly {
		return nil, nil
	}

	resBlock, err := b.TendermintBlockByHash(hash)
	if err != nil {
		return nil, err
	}

	if resBlock == nil {
		// block not found
		return nil, nil
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		b.logger.Debug("failed to fetch block result from Tendermint", "block-hash", hash.String(), "error", err.Error())
		return nil, nil
	}

	res, err := b.RPCBlockFromTendermintBlock(resBlock, blockRes, fullTx)
	if err != nil {
		b.logger.Debug("GetEthBlockFromTendermint failed", "hash", hash, "error", err.Error())
		return nil, err
	}

	return res, nil
}

// GetBlockTransactionCountByHash returns the number of Ethereum transactions in
// the block identified by hash.
func (b *Backend) GetBlockTransactionCountByHash(hash common.Hash) *hexutil.Uint {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if meta, err := b.cachedBlockMetaByHash(hash); err == nil && meta != nil {
		n := hexutil.Uint(meta.EthTxCount)
		return &n
	}

	sc, ok := b.clientCtx.Client.(cmrpcclient.SignClient)
	if !ok {
		b.logger.Error("invalid rpc client")
		return nil
	}
	block, err := sc.BlockByHash(b.ctx, hash.Bytes())
	if err != nil {
		b.logger.Debug("block not found", "hash", hash.Hex(), "error", err.Error())
		return nil
	} else if block == nil {
		b.logger.Debug("block not found", "hash", hash.Hex())
		return nil
	} else if block.Block == nil {
		b.logger.Debug("block not found", "hash", hash.Hex())
		return nil
	}

	return b.GetBlockTransactionCount(block)
}

// GetBlockTransactionCountByNumber returns the number of Ethereum transactions
// in the block identified by number.
func (b *Backend) GetBlockTransactionCountByNumber(blockNum rpctypes.BlockNumber) *hexutil.Uint {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if meta, err := b.cachedBlockMetaByNumber(blockNum); err == nil && meta != nil {
		n := hexutil.Uint(meta.EthTxCount)
		return &n
	}

	block, err := b.TendermintBlockByNumber(blockNum)
	if err != nil {
		b.logger.Debug("block not found", "height", blockNum.Int64(), "error", err.Error())
		return nil
	} else if block == nil {
		b.logger.Debug("block not found", "height", blockNum.Int64())
		return nil
	} else if block.Block == nil {
		b.logger.Debug("block not found", "height", blockNum.Int64())
		return nil
	}

	return b.GetBlockTransactionCount(block)
}

// GetBlockTransactionCount returns the number of Ethereum transactions in a
// given block.
func (b *Backend) GetBlockTransactionCount(block *cmrpctypes.ResultBlock) *hexutil.Uint {
	ethMsgs := b.EthMsgsFromTendermintBlock(block)
	n := hexutil.Uint(len(ethMsgs))
	return &n
}

// TendermintBlockByNumber returns a Tendermint-formatted block for a given
// block number
func (b *Backend) TendermintBlockByNumber(blockNum rpctypes.BlockNumber) (*cmrpctypes.ResultBlock, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	height := blockNum.Int64()
	if height <= 0 {
		// fetch the latest block number from the app state, more accurate than the tendermint block store state.
		n, err := b.BlockNumber()
		if err != nil {
			return nil, err
		}
		height = int64(n)
	}
	if b.clientCtx.Client == nil {
		return nil, errors.New("rpc client is nil")
	}
	resBlock, err := b.clientCtx.Client.Block(b.ctx, &height)
	if err != nil {
		b.logger.Debug("tendermint client failed to get block", "height", height, "error", err.Error())
		return nil, err
	}

	if resBlock.Block == nil {
		b.logger.Debug("TendermintBlockByNumber block not found", "height", height)
		return nil, nil
	}

	return resBlock, nil
}

// TendermintBlockResultByNumber returns a Tendermint-formatted block result
// by block number
func (b *Backend) TendermintBlockResultByNumber(height *int64) (*cmrpctypes.ResultBlockResults, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	sc, ok := b.clientCtx.Client.(cmrpcclient.SignClient)
	if !ok {
		return nil, errors.New("invalid rpc client")
	}
	return sc.BlockResults(b.ctx, height)
}

// TendermintBlockByHash returns a Tendermint-formatted block by block number
func (b *Backend) TendermintBlockByHash(blockHash common.Hash) (*cmrpctypes.ResultBlock, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	sc, ok := b.clientCtx.Client.(cmrpcclient.SignClient)
	if !ok {
		return nil, errors.New("invalid rpc client")
	}
	resBlock, err := sc.BlockByHash(b.ctx, blockHash.Bytes())
	if err != nil {
		b.logger.Debug("tendermint client failed to get block", "blockHash", blockHash.Hex(), "error", err.Error())
		return nil, err
	}

	if resBlock == nil || resBlock.Block == nil {
		b.logger.Debug("TendermintBlockByHash block not found", "blockHash", blockHash.Hex())
		return nil, nil
	}

	return resBlock, nil
}

// BlockNumberFromTendermint returns the BlockNumber from BlockNumberOrHash
func (b *Backend) BlockNumberFromTendermint(blockNrOrHash rpctypes.BlockNumberOrHash) (rpctypes.BlockNumber, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	switch {
	case blockNrOrHash.BlockHash == nil && blockNrOrHash.BlockNumber == nil:
		return rpctypes.EthEarliestBlockNumber, fmt.Errorf("types BlockHash and BlockNumber cannot be both nil")
	case blockNrOrHash.BlockHash != nil:
		blockNumber, err := b.BlockNumberFromTendermintByHash(*blockNrOrHash.BlockHash)
		if err != nil {
			return rpctypes.EthEarliestBlockNumber, err
		}
		return rpctypes.NewBlockNumber(blockNumber), nil
	case blockNrOrHash.BlockNumber != nil:
		return *blockNrOrHash.BlockNumber, nil
	default:
		return rpctypes.EthEarliestBlockNumber, nil
	}
}

// BlockNumberFromTendermintByHash returns the block height of given block hash
func (b *Backend) BlockNumberFromTendermintByHash(blockHash common.Hash) (*big.Int, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	meta, err := b.cachedBlockMetaByHash(blockHash)
	if err == nil && meta != nil {
		return big.NewInt(meta.Height), nil
	}
	if err != nil && !isIndexerCacheMiss(err) {
		if b.cfg.OfflineRPCOnly {
			return nil, err
		}
		b.logger.Debug("cached block-number-by-hash lookup failed; falling back to live rpc", "hash", blockHash.Hex(), "error", err.Error())
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.Errorf("block not found for hash %s", blockHash.Hex())
	}

	resBlock, err := b.TendermintBlockByHash(blockHash)
	if err != nil {
		return nil, err
	}
	if resBlock == nil {
		return nil, errors.Errorf("block not found for hash %s", blockHash.Hex())
	}
	return big.NewInt(resBlock.Block.Height), nil
}

// EthMsgsFromTendermintBlock returns all real MsgEthereumTxs from a Tendermint block.
func (b *Backend) EthMsgsFromTendermintBlock(
	resBlock *cmrpctypes.ResultBlock,
) []*evmtypes.MsgEthereumTx {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if resBlock == nil || resBlock.Block == nil {
		return nil
	}

	var result []*evmtypes.MsgEthereumTx
	block := resBlock.Block

	for _, tx := range block.Txs {
		decodedTx, err := b.clientCtx.TxConfig.TxDecoder()(tx)
		if err != nil {
			b.logger.Warn("failed to decode transaction in block", "height", block.Height, "error", err.Error())
			continue
		}

		for _, msg := range decodedTx.GetMsgs() {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				continue
			}

			result = append(result, ethMsg)
		}
	}

	return result
}

// HeaderByNumber returns the block header identified by height.
func (b *Backend) HeaderByNumber(blockNum rpctypes.BlockNumber) (*ethtypes.Header, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	meta, err := b.cachedBlockMetaByNumber(blockNum)
	if err == nil && meta != nil {
		return headerFromCachedBlockMeta(meta)
	}
	if err != nil && !isIndexerCacheMiss(err) {
		if b.cfg.OfflineRPCOnly {
			return nil, err
		}
		b.logger.Debug("cached header-by-number lookup failed; falling back to live rpc", "height", blockNum, "error", err.Error())
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.Errorf("block not found for height %d", blockNum)
	}

	resBlock, err := b.TendermintBlockByNumber(blockNum)
	if err != nil {
		return nil, err
	}

	if resBlock == nil {
		return nil, errors.Errorf("block not found for height %d", blockNum)
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		return nil, fmt.Errorf("block result not found for height %d", resBlock.Block.Height)
	}

	bloom, err := b.BlockBloom(blockRes)
	if err != nil {
		b.logger.Debug("HeaderByNumber BlockBloom failed", "height", resBlock.Block.Height)
	}

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle the error for pruned node.
		b.logger.Error("failed to fetch Base Fee from pruned block. Check node pruning configuration", "height", resBlock.Block.Height, "error", err)
	}

	ethHeader := rpctypes.EthHeaderFromTendermint(resBlock.Block.Header, bloom, baseFee)
	return ethHeader, nil
}

// HeaderByHash returns the block header identified by hash.
func (b *Backend) HeaderByHash(blockHash common.Hash) (*ethtypes.Header, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	meta, err := b.cachedBlockMetaByHash(blockHash)
	if err == nil && meta != nil {
		return headerFromCachedBlockMeta(meta)
	}
	if err != nil && !isIndexerCacheMiss(err) {
		if b.cfg.OfflineRPCOnly {
			return nil, err
		}
		b.logger.Debug("cached header-by-hash lookup failed; falling back to live rpc", "hash", blockHash.Hex(), "error", err.Error())
	}
	if b.cfg.OfflineRPCOnly {
		return nil, errors.Errorf("block not found for hash %s", blockHash.Hex())
	}

	resBlock, err := b.TendermintBlockByHash(blockHash)
	if err != nil {
		return nil, err
	}
	if resBlock == nil {
		return nil, errors.Errorf("block not found for hash %s", blockHash.Hex())
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		return nil, errors.Errorf("block result not found for height %d", resBlock.Block.Height)
	}

	bloom, err := b.BlockBloom(blockRes)
	if err != nil {
		b.logger.Debug("HeaderByHash BlockBloom failed", "height", resBlock.Block.Height)
	}

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle the error for pruned node.
		b.logger.Error("failed to fetch Base Fee from prunned block. Check node prunning configuration", "height", resBlock.Block.Height, "error", err)
	}

	ethHeader := rpctypes.EthHeaderFromTendermint(resBlock.Block.Header, bloom, baseFee)
	return ethHeader, nil
}

// BlockBloom query block bloom filter from block results
func (b *Backend) BlockBloom(blockRes *cmrpctypes.ResultBlockResults) (ethtypes.Bloom, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if blockRes == nil {
		return ethtypes.Bloom{}, errors.New("block results are nil")
	}

	if blockRes != nil && b.indexer != nil {
		meta, err := b.indexer.GetBlockMetaByHeight(blockRes.Height)
		if err == nil {
			return ethtypes.BytesToBloom(common.FromHex(meta.Bloom)), nil
		}
	}

	for _, event := range blockRes.FinalizeBlockEvents {
		if event.Type != evmtypes.EventTypeBlockBloom {
			continue
		}

		for _, attr := range event.Attributes {
			if bytes.Equal([]byte(attr.Key), bAttributeKeyEthereumBloom) {
				bloomBz := []byte(attr.Value)
				if len(bloomBz) == ethtypes.BloomByteLength {
					return ethtypes.BytesToBloom(bloomBz), nil
				}
				b.logger.Debug("block bloom attribute had unexpected length; deriving bloom from logs", "height", blockRes.Height, "length", len(bloomBz))
				break
			}
		}
	}

	logGroups, err := GetLogsFromBlockResults(blockRes)
	if err != nil {
		b.logger.Debug("BlockBloom event not found and log derivation failed", "height", blockRes.Height, "error", err.Error())
		return ethtypes.Bloom{}, errors.New("block bloom event is not found")
	}

	flatLogs := make([]*ethtypes.Log, 0)
	for _, group := range logGroups {
		flatLogs = append(flatLogs, group...)
	}
	return ethtypes.BytesToBloom(evmtypes.LogsBloom(flatLogs)), nil
}

// RPCBlockFromTendermintBlock returns a JSON-RPC compatible Ethereum block from a
// given Tendermint block and its block result.
func (b *Backend) RPCBlockFromTendermintBlock(
	resBlock *cmrpctypes.ResultBlock,
	blockRes *cmrpctypes.ResultBlockResults,
	fullTx bool,
) (map[string]interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if resBlock == nil || resBlock.Block == nil {
		return nil, fmt.Errorf("tendermint block is nil")
	}
	if blockRes == nil {
		return nil, fmt.Errorf("tendermint block result is nil")
	}

	ethRPCTxs := []interface{}{}
	block := resBlock.Block

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle the error for pruned node.
		b.logger.Error("failed to fetch Base Fee from prunned block. Check node prunning configuration", "height", block.Height, "error", err)
	}

	msgs := b.EthMsgsFromTendermintBlock(resBlock)
	for txIndex, ethMsg := range msgs {
		if !fullTx {
			ethRPCTxs = append(ethRPCTxs, ethMsg.Hash())
			continue
		}

		rpcTx, err := rpctypes.NewRPCTransaction(
			ethMsg,
			common.BytesToHash(block.Hash()),
			uint64(block.Height),
			uint64(txIndex),
			baseFee,
			b.ChainID().ToInt(),
		)
		if err != nil {
			b.logger.Debug("NewTransactionFromData for receipt failed", "hash", ethMsg.Hash, "error", err.Error())
			continue
		}
		ethRPCTxs = append(ethRPCTxs, rpcTx)
	}

	bloom, err := b.BlockBloom(blockRes)
	if err != nil {
		b.logger.Debug("failed to query BlockBloom", "height", block.Height, "error", err.Error())
	}

	req := &evmtypes.QueryValidatorAccountRequest{
		ConsAddress: sdk.ConsAddress(block.Header.ProposerAddress).String(),
	}

	var validatorAccAddr sdk.AccAddress

	queryCtx := b.contextWithHeight(block.Height)
	res, err := b.queryClient.ValidatorAccount(queryCtx, req)
	if err != nil {
		b.logger.Debug(
			"failed to query validator operator address",
			"height", block.Height,
			"cons-address", req.ConsAddress,
			"error", err.Error(),
		)
		// use zero address as the validator operator address
		validatorAccAddr = sdk.AccAddress(common.Address{}.Bytes())
	} else {
		validatorAccAddr, err = sdk.AccAddressFromBech32(res.AccountAddress)
		if err != nil {
			return nil, err
		}
	}

	validatorAddr := common.BytesToAddress(validatorAccAddr)

	gasLimit, err := rpctypes.BlockMaxGasFromConsensusParams(queryCtx, b.clientCtx, block.Height)
	if err != nil {
		b.logger.Error("failed to query consensus params", "error", err.Error())
	}

	var gasUsed uint64
	for _, txsResult := range blockRes.TxResults {
		gasUsed += uint64(txsResult.GetGasUsed())
	}

	formattedBlock := rpctypes.FormatBlock(
		block.Header, block.Size(),
		gasLimit, new(big.Int).SetUint64(gasUsed),
		ethRPCTxs, bloom, validatorAddr, baseFee,
	)
	return formattedBlock, nil
}

// EthBlockByNumber returns the Ethereum Block identified by number.
func (b *Backend) EthBlockByNumber(blockNum rpctypes.BlockNumber) (*ethtypes.Block, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	resBlock, err := b.TendermintBlockByNumber(blockNum)
	if err != nil {
		return nil, err
	}
	if resBlock == nil {
		// block not found
		return nil, fmt.Errorf("block not found for height %d", blockNum)
	}

	blockRes, err := b.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		return nil, fmt.Errorf("block result not found for height %d", resBlock.Block.Height)
	}

	return b.EthBlockFromTendermintBlock(resBlock, blockRes)
}

// EthBlockFromTendermintBlock returns an Ethereum Block type from Tendermint block
// EthBlockFromTendermintBlock
func (b *Backend) EthBlockFromTendermintBlock(
	resBlock *cmrpctypes.ResultBlock,
	blockRes *cmrpctypes.ResultBlockResults,
) (*ethtypes.Block, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	if resBlock == nil || resBlock.Block == nil {
		return nil, fmt.Errorf("tendermint block is nil")
	}
	if blockRes == nil {
		return nil, fmt.Errorf("tendermint block result is nil")
	}

	block := resBlock.Block
	height := block.Height
	bloom, err := b.BlockBloom(blockRes)
	if err != nil {
		b.logger.Debug("HeaderByNumber BlockBloom failed", "height", height)
	}

	baseFee, err := b.BaseFee(blockRes)
	if err != nil {
		// handle error for pruned node and log
		b.logger.Error("failed to fetch Base Fee from prunned block. Check node prunning configuration", "height", height, "error", err)
	}

	ethHeader := rpctypes.EthHeaderFromTendermint(block.Header, bloom, baseFee)
	msgs := b.EthMsgsFromTendermintBlock(resBlock)

	txs := make([]*ethtypes.Transaction, len(msgs))
	for i, ethMsg := range msgs {
		txs[i] = ethMsg.AsTransaction()
	}

	// TODO: add tx receipts
	// TODO(max): check if this still needed
	ethBlock := ethtypes.NewBlockWithHeader(ethHeader).WithBody(ethtypes.Body{Transactions: txs})

	return ethBlock, nil
}

func (b *Backend) cachedBlockMetaByNumber(blockNum rpctypes.BlockNumber) (*txindexer.CachedBlockMeta, error) {
	if b.indexer == nil {
		return nil, nil
	}

	height, err := b.indexedBlockHeight(blockNum)
	if err != nil || height < 1 {
		return nil, err
	}
	return b.indexer.GetBlockMetaByHeight(height)
}

func (b *Backend) cachedBlockMetaByHash(hash common.Hash) (*txindexer.CachedBlockMeta, error) {
	if b.indexer == nil {
		return nil, nil
	}
	return b.indexer.GetBlockMetaByHash(hash)
}

func (b *Backend) indexedBlockHeight(blockNum rpctypes.BlockNumber) (int64, error) {
	if b.indexer == nil {
		return -1, nil
	}
	if blockNum < 0 {
		return b.indexer.LastIndexedBlock()
	}
	return blockNum.Int64(), nil
}

func (b *Backend) rpcBlockFromCachedMeta(meta *txindexer.CachedBlockMeta, fullTx bool) (map[string]interface{}, error) {
	if err := validateCachedBlockMeta(meta); err != nil {
		return nil, err
	}

	transactions, err := b.cachedBlockTransactions(meta, fullTx)
	if err != nil {
		return nil, err
	}

	bloom := ethtypes.Bloom{}
	if meta.Bloom != "" {
		bloom = ethtypes.BytesToBloom(common.FromHex(meta.Bloom))
	}
	gasUsed := new(big.Int).SetUint64(meta.GasUsed)
	blockHash := common.HexToHash(meta.Hash)
	parentHash := common.HexToHash(meta.ParentHash)
	stateRoot := hexutil.Bytes(common.FromHex(meta.StateRoot))
	miner := common.HexToAddress(meta.Miner)
	transactionsRoot := ethtypes.EmptyRootHash
	if meta.TransactionsRoot != "" {
		transactionsRoot = common.HexToHash(meta.TransactionsRoot)
	} else if len(transactions) > 0 {
		transactionsRoot = common.Hash{}
	}

	block := map[string]interface{}{
		"number":           hexutil.Uint64(meta.Height),
		"hash":             hexutil.Bytes(blockHash.Bytes()),
		"parentHash":       parentHash,
		"nonce":            ethtypes.BlockNonce{},
		"sha3Uncles":       ethtypes.EmptyUncleHash,
		"logsBloom":        bloom,
		"stateRoot":        stateRoot,
		"miner":            miner,
		"mixHash":          common.Hash{},
		"difficulty":       (*hexutil.Big)(big.NewInt(0)),
		"extraData":        "0x",
		"size":             hexutil.Uint64(meta.Size),
		"gasLimit":         hexutil.Uint64(meta.GasLimit),
		"gasUsed":          (*hexutil.Big)(gasUsed),
		"timestamp":        hexutil.Uint64(meta.Timestamp),
		"transactionsRoot": transactionsRoot,
		"receiptsRoot":     ethtypes.EmptyRootHash,
		"uncles":           []common.Hash{},
		"transactions":     transactions,
		"totalDifficulty":  (*hexutil.Big)(big.NewInt(0)),
	}

	if meta.BaseFee != "" {
		baseFee, err := hexutil.DecodeBig(meta.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("cached block meta has invalid base fee for height %d: %w", meta.Height, err)
		}
		block["baseFeePerGas"] = (*hexutil.Big)(baseFee)
	}

	return block, nil
}

func (b *Backend) cachedBlockTransactions(meta *txindexer.CachedBlockMeta, fullTx bool) ([]interface{}, error) {
	if meta == nil {
		return nil, fmt.Errorf("cached block meta is nil")
	}
	if meta.EthTxCount < 0 {
		return nil, fmt.Errorf("cached block meta has invalid eth tx count for height %d: %d", meta.Height, meta.EthTxCount)
	}
	if meta.EthTxCount == 0 {
		return []interface{}{}, nil
	}
	if b.indexer == nil {
		return nil, fmt.Errorf("cached block transactions unavailable without indexer for height %d", meta.Height)
	}

	hashes, err := b.indexer.GetRPCTransactionHashesByBlockHeight(meta.Height)
	if err != nil {
		return nil, err
	}
	if len(hashes) != int(meta.EthTxCount) {
		return nil, fmt.Errorf(
			"cached block tx count mismatch at height %d: expected %d got %d",
			meta.Height,
			meta.EthTxCount,
			len(hashes),
		)
	}

	transactions := make([]interface{}, 0, len(hashes))
	blockHash := common.HexToHash(meta.Hash)
	for idx, hash := range hashes {
		if hash == (common.Hash{}) {
			return nil, fmt.Errorf("cached block tx hash missing for height %d", meta.Height)
		}
		if !fullTx {
			transactions = append(transactions, hash)
			continue
		}

		rpcTx, err := b.indexer.GetRPCTransactionByHash(hash)
		if err != nil {
			return nil, err
		}
		if rpcTx == nil {
			return nil, fmt.Errorf("cached block tx missing for height %d hash %s", meta.Height, hash.Hex())
		}
		if rpcTx.BlockHash == nil || *rpcTx.BlockHash != blockHash {
			return nil, fmt.Errorf("cached block tx has invalid block hash for height %d hash %s", meta.Height, hash.Hex())
		}
		if rpcTx.BlockNumber == nil || (*big.Int)(rpcTx.BlockNumber).Int64() != meta.Height {
			return nil, fmt.Errorf("cached block tx has invalid block number for height %d hash %s", meta.Height, hash.Hex())
		}
		if rpcTx.TransactionIndex == nil {
			return nil, fmt.Errorf("cached block tx has nil transaction index for height %d hash %s", meta.Height, hash.Hex())
		}
		if uint64(*rpcTx.TransactionIndex) != uint64(idx) {
			return nil, fmt.Errorf(
				"cached block tx has unexpected transaction index for height %d hash %s: expected %d got %d",
				meta.Height,
				hash.Hex(),
				idx,
				uint64(*rpcTx.TransactionIndex),
			)
		}
		transactions = append(transactions, rpcTx)
	}
	return transactions, nil
}

func headerFromCachedBlockMeta(meta *txindexer.CachedBlockMeta) (*ethtypes.Header, error) {
	if err := validateCachedBlockMeta(meta); err != nil {
		return nil, err
	}

	header := &ethtypes.Header{
		ParentHash:  common.HexToHash(meta.ParentHash),
		UncleHash:   ethtypes.EmptyUncleHash,
		Coinbase:    common.HexToAddress(meta.Miner),
		Root:        common.BytesToHash(common.FromHex(meta.StateRoot)),
		TxHash:      ethtypes.EmptyRootHash,
		ReceiptHash: ethtypes.EmptyRootHash,
		Bloom:       ethtypes.BytesToBloom(common.FromHex(meta.Bloom)),
		Difficulty:  big.NewInt(0),
		Number:      big.NewInt(meta.Height),
		GasLimit:    meta.GasLimit,
		GasUsed:     meta.GasUsed,
		Time:        uint64(meta.Timestamp),
		Extra:       []byte{},
		MixDigest:   common.Hash{},
		Nonce:       ethtypes.BlockNonce{},
	}
	if meta.TransactionsRoot != "" {
		header.TxHash = common.HexToHash(meta.TransactionsRoot)
	}
	if meta.BaseFee != "" {
		baseFee, err := hexutil.DecodeBig(meta.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("cached block meta has invalid base fee for height %d: %w", meta.Height, err)
		}
		header.BaseFee = baseFee
	}
	return header, nil
}

func isIndexerCacheMiss(err error) bool {
	return errors.Is(err, txindexer.ErrCacheMiss)
}

func validateCachedBlockMeta(meta *txindexer.CachedBlockMeta) error {
	if meta == nil {
		return fmt.Errorf("cached block meta is nil")
	}
	if meta.Height < 1 {
		return fmt.Errorf("cached block meta has invalid height: %d", meta.Height)
	}
	if meta.EthTxCount < 0 {
		return fmt.Errorf("cached block meta has invalid eth tx count for height %d: %d", meta.Height, meta.EthTxCount)
	}
	if meta.TxCount < 0 {
		return fmt.Errorf("cached block meta has invalid tx count for height %d: %d", meta.Height, meta.TxCount)
	}
	if !isHexHashString(meta.Hash) {
		return fmt.Errorf("cached block meta has invalid hash for height %d: %q", meta.Height, meta.Hash)
	}
	if meta.ParentHash != "" && !isHexHashString(meta.ParentHash) {
		return fmt.Errorf("cached block meta has invalid parent hash for height %d: %q", meta.Height, meta.ParentHash)
	}
	if meta.StateRoot == "" || !isHexHashString(meta.StateRoot) {
		return fmt.Errorf("cached block meta has invalid state root for height %d: %q", meta.Height, meta.StateRoot)
	}
	if meta.Miner == "" || !common.IsHexAddress(meta.Miner) {
		return fmt.Errorf("cached block meta has invalid miner for height %d: %q", meta.Height, meta.Miner)
	}
	if meta.TransactionsRoot == "" && meta.EthTxCount > 0 {
		return fmt.Errorf("cached block meta missing transactions root for height %d", meta.Height)
	}
	if meta.TransactionsRoot != "" && !isHexHashString(meta.TransactionsRoot) {
		return fmt.Errorf("cached block meta has invalid transactions root for height %d: %q", meta.Height, meta.TransactionsRoot)
	}
	if meta.Bloom != "" {
		bloomBytes, err := hexutil.Decode(meta.Bloom)
		if err != nil || len(bloomBytes) != ethtypes.BloomByteLength {
			return fmt.Errorf("cached block meta has invalid bloom length for height %d: %d", meta.Height, len(bloomBytes))
		}
	}
	if meta.BaseFee != "" {
		if _, err := hexutil.DecodeBig(meta.BaseFee); err != nil {
			return fmt.Errorf("cached block meta has invalid base fee for height %d: %w", meta.Height, err)
		}
	}
	return nil
}

func isHexHashString(value string) bool {
	bz, err := hexutil.Decode(value)
	return err == nil && len(bz) == common.HashLength
}
