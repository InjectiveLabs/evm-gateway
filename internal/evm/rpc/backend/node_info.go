package backend

import (
	"math/big"
	"time"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"upd.dev/xlab/gotracer"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// Syncing returns false in case the node is currently not syncing with the network. It can be up to date or has not
// yet received the latest block headers from its pears. In case it is synchronizing:
// - startingBlock: block number this node started to synchronize from
// - currentBlock:  block number this node is currently importing
// - highestBlock:  block number of the highest block header this node has received from peers
// - pulledStates:  number of state entries processed until now
// - knownStates:   number of known state entries that still need to be pulled
func (b *Backend) Syncing() (interface{}, error) {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	status, err := b.clientCtx.Client.Status(b.ctx)
	if err != nil {
		return false, err
	}

	if !status.SyncInfo.CatchingUp {
		return false, nil
	}

	return map[string]interface{}{
		"startingBlock": hexutil.Uint64(status.SyncInfo.EarliestBlockHeight),
		"currentBlock":  hexutil.Uint64(status.SyncInfo.LatestBlockHeight),
		// "highestBlock":  nil, // NA
		// "pulledStates":  nil, // NA
		// "knownStates":   nil, // NA
	}, nil
}

// SetEtherbase sets the etherbase of the miner
func (b *Backend) SetEtherbase(etherbase common.Address) bool {
	b.logger.Warn("miner_setEtherbase is disabled in evm-gateway", "etherbase", etherbase.Hex())
	return false
}

// SetGasPrice sets the minimum accepted gas price for the miner.
// NOTE: this function accepts only integers to have the same interface than go-eth
// to use float values, the gas prices must be configured using the configuration file
func (b *Backend) SetGasPrice(gasPrice hexutil.Big) bool {
	var unit string
	minGasPrices := b.cfg.GetMinGasPrices()

	// fetch the base denom from the sdk Config in case it's not currently defined on the node config
	if len(minGasPrices) == 0 || minGasPrices.Empty() {
		var err error
		unit, err = sdk.GetBaseDenom()
		if err != nil {
			b.logger.Debug("could not get the denom of smallest unit registered", "error", err.Error())
			return false
		}
	} else {
		unit = minGasPrices[0].Denom
	}

	c := sdk.NewDecCoin(unit, sdkmath.NewIntFromBigInt(gasPrice.ToInt()))

	b.cfg.SetMinGasPrices(sdk.DecCoins{c})
	b.logger.Info("min gas price updated for current process only", "gas-price", c.String())
	return true
}

// UnprotectedAllowed returns the node configuration value for allowing
// unprotected transactions (i.e not replay-protected)
func (b Backend) UnprotectedAllowed() bool {
	return b.allowUnprotectedTxs
}

// RPCGasCap is the global gas cap for eth-call variants.
func (b *Backend) RPCGasCap() uint64 {
	return b.cfg.JSONRPC.GasCap
}

// RPCEVMTimeout is the global evm timeout for eth-call variants.
func (b *Backend) RPCEVMTimeout() time.Duration {
	return b.cfg.JSONRPC.EVMTimeout
}

// RPCGasCap is the global gas cap for eth-call variants.
func (b *Backend) RPCTxFeeCap() float64 {
	return b.cfg.JSONRPC.TxFeeCap
}

// RPCFilterCap is the limit for total number of filters that can be created
func (b *Backend) RPCFilterCap() int32 {
	return b.cfg.JSONRPC.FilterCap
}

// RPCFeeHistoryCap is the limit for total number of blocks that can be fetched
func (b *Backend) RPCFeeHistoryCap() int32 {
	return b.cfg.JSONRPC.FeeHistoryCap
}

// RPCLogsCap defines the max number of results can be returned from single `eth_getLogs` query.
func (b *Backend) RPCLogsCap() int32 {
	return b.cfg.JSONRPC.LogsCap
}

// RPCBlockRangeCap defines the max block range allowed for `eth_getLogs` query.
func (b *Backend) RPCBlockRangeCap() int32 {
	return b.cfg.JSONRPC.BlockRangeCap
}

// RPCMinGasPrice returns the minimum gas price for a transaction obtained from
// the node config. If set value is 0, it will default to 20.

func (b *Backend) RPCMinGasPrice() *big.Int {
	ctx := b.operationContext()
	if b.ctx != nil {
		defer gotracer.Trace(&ctx, b.baseTraceTags)()
	} else {
		defer gotracer.Traceless(&ctx, b.baseTraceTags)()
	}
	b = b.WithContext(ctx).(*Backend)

	evmParams, err := b.queryClient.Params(b.ctx, &evmtypes.QueryParamsRequest{})
	if err != nil {
		return new(big.Int).SetInt64(evmtypes.DefaultGasPrice)
	}

	minGasPrice := b.cfg.GetMinGasPrices()
	amt := minGasPrice.AmountOf(evmParams.Params.EvmDenom).TruncateInt()
	if amt.IsZero() {
		return new(big.Int).SetInt64(evmtypes.DefaultGasPrice)
	}

	return amt.BigInt()
}
