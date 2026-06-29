package backend

import (
	"errors"
	"math/big"
	"testing"

	txsigning "cosmossdk.io/x/tx/signing"
	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	protov2 "google.golang.org/protobuf/proto"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestGetTransactionReceiptDynamicFeeEffectiveGasPrice(t *testing.T) {
	fixture := newDynamicFeeReceiptFixture(t)
	db := dbm.NewMemDB()
	kv := indexer.NewKVIndexer(
		db,
		backendTestLogger(),
		client.Context{TxConfig: backendReceiptTestTxConfig{tx: fixture.decodedTx}},
	)
	require.NoError(t, kv.IndexBlockWithResults(fixture.block, fixture.blockResults))
	require.NoError(t, db.Delete(indexer.ReceiptKey(fixture.txHash)))

	b := newReceiptTestBackend(t, fixture, kv)

	receipt, err := b.GetTransactionReceipt(fixture.txHash)

	require.NoError(t, err)
	require.NotNil(t, receipt)
	assertEffectiveGasPrice(t, receipt, fixture.expectedEffectiveGasPrice, fixture.feeCap)
}

func TestGetBlockReceiptsDynamicFeeEffectiveGasPrice(t *testing.T) {
	fixture := newDynamicFeeReceiptFixture(t)
	b := newReceiptTestBackend(t, fixture, nil)
	blockNumber := rpctypes.BlockNumber(fixture.block.Height)

	receipts, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber})

	require.NoError(t, err)
	require.Len(t, receipts, 1)
	assertEffectiveGasPrice(t, receipts[0], fixture.expectedEffectiveGasPrice, fixture.feeCap)
}

func assertEffectiveGasPrice(t *testing.T, receipt map[string]interface{}, expected *big.Int, feeCap *big.Int) {
	t.Helper()

	effectiveGasPrice, ok := receipt["effectiveGasPrice"].(*hexutil.Big)
	require.True(t, ok)
	require.Equal(t, 0, (*big.Int)(effectiveGasPrice).Cmp(expected))
	require.NotEqual(t, 0, (*big.Int)(effectiveGasPrice).Cmp(feeCap))
}

type dynamicFeeReceiptFixture struct {
	chainID                   *big.Int
	baseFee                   *big.Int
	feeCap                    *big.Int
	tipCap                    *big.Int
	expectedEffectiveGasPrice *big.Int
	msg                       *evmtypes.MsgEthereumTx
	txHash                    common.Hash
	decodedTx                 sdk.Tx
	block                     *tmtypes.Block
	blockResults              *cmrpctypes.ResultBlockResults
}

func newDynamicFeeReceiptFixture(t *testing.T) dynamicFeeReceiptFixture {
	t.Helper()

	chainID := big.NewInt(1337)
	baseFee := big.NewInt(160000000)
	feeCap := big.NewInt(1716000000)
	tipCap := big.NewInt(1500000000)
	expectedEffectiveGasPrice := big.NewInt(1660000000)
	msg := signedDynamicFeeMsg(t, chainID, feeCap, tipCap)
	txHash := msg.Hash()
	decodedTx := backendReceiptTestSDKTx{
		msgs:             []sdk.Msg{msg},
		extensionOptions: backendReceiptTestEVMExtensionOptions(),
	}
	height := int64(1)
	txBz := tmtypes.Tx("eth-tx")
	block := tmtypes.MakeBlock(height, []tmtypes.Tx{txBz}, nil, nil)
	blockResults := &cmrpctypes.ResultBlockResults{
		Height: height,
		TxResults: []*abci.ExecTxResult{
			{
				Code:    abci.CodeTypeOK,
				GasUsed: 21000,
				Data: mustMarshalBackendTxMsgData(t, &evmtypes.MsgEthereumTxResponse{
					Hash: txHash.Hex(),
				}),
				Events: []abci.Event{ethereumTxEvent(txHash, 0, 21000)},
			},
		},
		FinalizeBlockEvents: []abci.Event{backendTestBaseFeeEvent(baseFee)},
	}

	return dynamicFeeReceiptFixture{
		chainID:                   chainID,
		baseFee:                   baseFee,
		feeCap:                    feeCap,
		tipCap:                    tipCap,
		expectedEffectiveGasPrice: expectedEffectiveGasPrice,
		msg:                       msg,
		txHash:                    txHash,
		decodedTx:                 decodedTx,
		block:                     block,
		blockResults:              blockResults,
	}
}

func newReceiptTestBackend(t *testing.T, fixture dynamicFeeReceiptFixture, kv indexer.TxIndexer) *Backend {
	t.Helper()

	rpcClient := &mocks.Client{}
	height := fixture.block.Height
	rpcClient.On("Block", mock.Anything, mock.MatchedBy(func(got *int64) bool {
		return got != nil && *got == height
	})).Return(&cmrpctypes.ResultBlock{Block: fixture.block}, nil)
	rpcClient.On("BlockResults", mock.Anything, mock.MatchedBy(func(got *int64) bool {
		return got != nil && *got == height
	})).Return(fixture.blockResults, nil)
	t.Cleanup(func() { rpcClient.AssertExpectations(t) })

	b := NewBackend(
		backendTestLogger(),
		appconfig.Config{ChainID: "injective-1", EVMChainID: fixture.chainID.String()},
		client.Context{
			Client:   rpcClient,
			TxConfig: backendReceiptTestTxConfig{tx: fixture.decodedTx},
		},
		client.Context{
			Client:   rpcClient,
			TxConfig: backendReceiptTestTxConfig{tx: fixture.decodedTx},
		},
		false,
		kv,
		nil,
	)
	b.queryClient = &rpctypes.QueryClient{
		TxFeesQueryClient: backendTestTxFeesQueryClient{err: errors.New("query unavailable")},
	}
	return b
}

func signedDynamicFeeMsg(t *testing.T, chainID, feeCap, tipCap *big.Int) *evmtypes.MsgEthereumTx {
	t.Helper()

	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe51296170827965fb7b05b8a9e7f6f2")
	require.NoError(t, err)
	to := common.HexToAddress("0x1000000000000000000000000000000000000001")
	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     0,
		To:        &to,
		Value:     big.NewInt(0),
		Gas:       21000,
		GasFeeCap: new(big.Int).Set(feeCap),
		GasTipCap: new(big.Int).Set(tipCap),
	})
	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, key)
	require.NoError(t, err)

	msg := &evmtypes.MsgEthereumTx{}
	require.NoError(t, msg.FromSignedEthereumTx(signedTx, signer))
	return msg
}

func ethereumTxEvent(txHash common.Hash, txIndex int32, gasUsed uint64) abci.Event {
	return abci.Event{
		Type: evmtypes.EventTypeEthereumTx,
		Attributes: []abci.EventAttribute{
			{Key: evmtypes.AttributeKeyEthereumTxHash, Value: txHash.Hex()},
			{Key: evmtypes.AttributeKeyTxIndex, Value: new(big.Int).SetInt64(int64(txIndex)).String()},
			{Key: evmtypes.AttributeKeyTxGasUsed, Value: new(big.Int).SetUint64(gasUsed).String()},
		},
	}
}

type backendReceiptTestSDKTx struct {
	msgs             []sdk.Msg
	extensionOptions []*codectypes.Any
}

func (t backendReceiptTestSDKTx) GetMsgs() []sdk.Msg {
	return t.msgs
}

func (t backendReceiptTestSDKTx) GetMsgsV2() ([]protov2.Message, error) {
	return nil, nil
}

func (t backendReceiptTestSDKTx) GetExtensionOptions() []*codectypes.Any {
	return t.extensionOptions
}

func (t backendReceiptTestSDKTx) GetNonCriticalExtensionOptions() []*codectypes.Any {
	return nil
}

func backendReceiptTestEVMExtensionOptions() []*codectypes.Any {
	return []*codectypes.Any{
		{TypeUrl: "/injective.evm.v1.ExtensionOptionsEthereumTx"},
	}
}

type backendReceiptTestTxConfig struct {
	tx sdk.Tx
}

func (c backendReceiptTestTxConfig) TxEncoder() sdk.TxEncoder {
	return func(tx sdk.Tx) ([]byte, error) { return nil, nil }
}

func (c backendReceiptTestTxConfig) TxDecoder() sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, error) { return c.tx, nil }
}

func (c backendReceiptTestTxConfig) TxJSONEncoder() sdk.TxEncoder {
	return c.TxEncoder()
}

func (c backendReceiptTestTxConfig) TxJSONDecoder() sdk.TxDecoder {
	return c.TxDecoder()
}

func (c backendReceiptTestTxConfig) MarshalSignatureJSON([]signing.SignatureV2) ([]byte, error) {
	return nil, nil
}

func (c backendReceiptTestTxConfig) UnmarshalSignatureJSON([]byte) ([]signing.SignatureV2, error) {
	return nil, nil
}

func (c backendReceiptTestTxConfig) NewTxBuilder() client.TxBuilder {
	return nil
}

func (c backendReceiptTestTxConfig) WrapTxBuilder(sdk.Tx) (client.TxBuilder, error) {
	return nil, nil
}

func (c backendReceiptTestTxConfig) SignModeHandler() *txsigning.HandlerMap {
	return nil
}

func (c backendReceiptTestTxConfig) SigningContext() *txsigning.Context {
	return nil
}
