package types

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	txsigning "cosmossdk.io/x/tx/signing"
	abci "github.com/cometbft/cometbft/abci/types"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/mock"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

type stubTxConfig struct {
	decoder sdk.TxDecoder
}

type cometRPCClientWrapper struct {
	*rpcmocks.Client
}

func (c *cometRPCClientWrapper) SetLogger(cmtlog.Logger) {}

var _ cmrpcclient.Client = (*cometRPCClientWrapper)(nil)

func (s stubTxConfig) TxEncoder() sdk.TxEncoder     { return nil }
func (s stubTxConfig) TxDecoder() sdk.TxDecoder     { return s.decoder }
func (s stubTxConfig) TxJSONEncoder() sdk.TxEncoder { return nil }
func (s stubTxConfig) TxJSONDecoder() sdk.TxDecoder { return nil }
func (s stubTxConfig) MarshalSignatureJSON([]signingtypes.SignatureV2) ([]byte, error) {
	return nil, nil
}
func (s stubTxConfig) UnmarshalSignatureJSON([]byte) ([]signingtypes.SignatureV2, error) {
	return nil, nil
}
func (s stubTxConfig) NewTxBuilder() client.TxBuilder                 { return nil }
func (s stubTxConfig) WrapTxBuilder(sdk.Tx) (client.TxBuilder, error) { return nil, nil }
func (s stubTxConfig) SignModeHandler() *txsigning.HandlerMap         { return nil }
func (s stubTxConfig) SigningContext() *txsigning.Context             { return nil }

func TestRawTxToEthTx(t *testing.T) {
	ethMsgA := evmtypes.NewTx(big.NewInt(1), 1, nil, big.NewInt(0), 21000, big.NewInt(1), nil, nil, nil, nil)
	ethMsgB := evmtypes.NewTx(big.NewInt(1), 2, nil, big.NewInt(0), 22000, big.NewInt(2), nil, nil, nil, nil)

	ctx := client.Context{}.WithTxConfig(stubTxConfig{
		decoder: func([]byte) (sdk.Tx, error) {
			return mockTx{msgs: []sdk.Msg{ethMsgA, ethMsgB}}, nil
		},
	})
	got, err := RawTxToEthTx(ctx, cmtypes.Tx("ignored"))
	if err != nil {
		t.Fatalf("RawTxToEthTx returned error: %v", err)
	}
	if len(got) != 2 || got[0] != ethMsgA || got[1] != ethMsgB {
		t.Fatalf("unexpected eth txs: %#v", got)
	}

	ctx = client.Context{}.WithTxConfig(stubTxConfig{
		decoder: func([]byte) (sdk.Tx, error) {
			return mockTx{msgs: []sdk.Msg{&txtypes.Tx{}}}, nil
		},
	})
	if _, err := RawTxToEthTx(ctx, cmtypes.Tx("ignored")); err == nil {
		t.Fatal("expected invalid message type error")
	}

	ctx = client.Context{}.WithTxConfig(stubTxConfig{
		decoder: func([]byte) (sdk.Tx, error) {
			return nil, errors.New("decode failed")
		},
	})
	if _, err := RawTxToEthTx(ctx, cmtypes.Tx("ignored")); err == nil || err.Error() == "" {
		t.Fatal("expected decode error")
	}
}

func TestEthHeaderFromTendermintAndFormatBlock(t *testing.T) {
	header := cmtypes.Header{
		Height:          7,
		Time:            time.Unix(1700000000, 0).UTC(),
		LastBlockID:     cmtypes.BlockID{Hash: cmbytes.HexBytes{0x11}},
		DataHash:        cmbytes.HexBytes{0x22},
		AppHash:         cmbytes.HexBytes{0x33},
		ProposerAddress: cmbytes.HexBytes{0x44},
	}
	bloom := ethtypes.BytesToBloom([]byte{0xaa})
	baseFee := big.NewInt(99)
	ethHeader := EthHeaderFromTendermint(header, bloom, baseFee)
	if ethHeader.Number.Cmp(big.NewInt(7)) != 0 || ethHeader.Time != uint64(header.Time.Unix()) || ethHeader.BaseFee.Cmp(baseFee) != 0 {
		t.Fatalf("unexpected ethereum header: %+v", ethHeader)
	}
	if ethHeader.TxHash != ethtypes.EmptyRootHash {
		t.Fatalf("expected non-empty data hash to keep empty root hash, got %s", ethHeader.TxHash.Hex())
	}

	header.DataHash = nil
	ethHeader = EthHeaderFromTendermint(header, bloom, nil)
	if ethHeader.TxHash != (common.Hash{}) {
		t.Fatalf("expected empty data hash to map to zero hash, got %s", ethHeader.TxHash.Hex())
	}

	block := FormatBlock(
		header,
		128,
		9000000,
		big.NewInt(1234),
		nil,
		bloom,
		common.HexToAddress("0x5000000000000000000000000000000000000005"),
		nil,
	)
	if block["transactionsRoot"] != ethtypes.EmptyRootHash {
		t.Fatalf("unexpected transactions root for empty txs: %#v", block["transactionsRoot"])
	}
	if block["baseFeePerGas"] != nil {
		t.Fatalf("did not expect base fee field when nil: %#v", block["baseFeePerGas"])
	}

	block = FormatBlock(
		header,
		256,
		8000000,
		big.NewInt(55),
		[]interface{}{"tx"},
		bloom,
		common.HexToAddress("0x6000000000000000000000000000000000000006"),
		big.NewInt(77),
	)
	if block["transactionsRoot"] != common.BytesToHash(header.DataHash) {
		t.Fatalf("unexpected transactions root: %#v", block["transactionsRoot"])
	}
	if _, ok := block["baseFeePerGas"]; !ok {
		t.Fatal("expected baseFeePerGas field")
	}
}

func TestBlockMaxGasFromConsensusParams(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-Comet client")
		}
	}()
	_, _ = BlockMaxGasFromConsensusParams(context.Background(), client.Context{}, 10)
}

func TestBlockMaxGasFromConsensusParamsWithMockClient(t *testing.T) {
	height := int64(55)
	mockClient := &cometRPCClientWrapper{Client: &rpcmocks.Client{}}
	mockClient.On("ConsensusParams", mock.Anything, &height).Return(&coretypes.ResultConsensusParams{
		ConsensusParams: cmtypes.ConsensusParams{
			Block: cmtypes.BlockParams{MaxGas: -1},
		},
	}, nil).Once()

	clientCtx := client.Context{}.WithClient(mockClient)
	got, err := BlockMaxGasFromConsensusParams(context.Background(), clientCtx, height)
	if err != nil {
		t.Fatalf("BlockMaxGasFromConsensusParams returned error: %v", err)
	}
	if got != int64(^uint32(0)) {
		t.Fatalf("unexpected unlimited gas mapping: %d", got)
	}

	height = 56
	clientCtx = client.Context{}.WithClient(mockClient)
	mockClient.On("ConsensusParams", mock.Anything, &height).Return((*coretypes.ResultConsensusParams)(nil), errors.New("boom")).Once()
	if _, err := BlockMaxGasFromConsensusParams(context.Background(), clientCtx, height); err == nil {
		t.Fatal("expected consensus params error")
	}
}

func TestNewRPCTransactionAndHelpers(t *testing.T) {
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe512961708279cf9f6f7d9e9f3ecf4a")
	if err != nil {
		t.Fatalf("HexToECDSA: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	chainID := big.NewInt(7)
	to := common.HexToAddress("0x7000000000000000000000000000000000000007")

	legacyTx := ethtypes.NewTx(&ethtypes.LegacyTx{
		Nonce:    1,
		To:       &to,
		Value:    big.NewInt(11),
		Gas:      21000,
		GasPrice: big.NewInt(100),
		Data:     []byte{0x1},
	})
	legacySigned, err := ethtypes.SignTx(legacyTx, ethtypes.HomesteadSigner{}, key)
	if err != nil {
		t.Fatalf("SignTx legacy: %v", err)
	}
	legacyMsg := &evmtypes.MsgEthereumTx{}
	if err := legacyMsg.FromSignedEthereumTx(legacySigned, ethtypes.HomesteadSigner{}); err != nil {
		t.Fatalf("FromSignedEthereumTx legacy: %v", err)
	}

	rpcTx, err := NewTransactionFromMsg(legacyMsg, common.Hash{}, 0, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewTransactionFromMsg returned error: %v", err)
	}
	if rpcTx.Type != ethtypes.LegacyTxType || rpcTx.BlockHash != nil || rpcTx.From != from || rpcTx.To == nil || *rpcTx.To != to {
		t.Fatalf("unexpected legacy rpc tx: %+v", rpcTx)
	}

	_, accessMsg := mustSignedMsg(t, &ethtypes.AccessListTx{
		ChainID:    chainID,
		Nonce:      2,
		To:         &to,
		Value:      big.NewInt(12),
		Gas:        30000,
		GasPrice:   big.NewInt(200),
		Data:       []byte{0x2},
		AccessList: ethtypes.AccessList{{Address: to}},
	}, ethtypes.LatestSignerForChainID(chainID), key)

	blockHash := common.HexToHash("0x8000000000000000000000000000000000000000000000000000000000000008")
	rpcTx, err = NewRPCTransaction(accessMsg, blockHash, 22, 3, nil, chainID)
	if err != nil {
		t.Fatalf("NewRPCTransaction access-list returned error: %v", err)
	}
	if rpcTx.BlockHash == nil || *rpcTx.BlockHash != blockHash || rpcTx.TransactionIndex == nil || uint64(*rpcTx.TransactionIndex) != 3 {
		t.Fatalf("unexpected block metadata: %+v", rpcTx)
	}
	if rpcTx.Accesses == nil || len(*rpcTx.Accesses) != 1 || rpcTx.ChainID.ToInt().Cmp(chainID) != 0 {
		t.Fatalf("unexpected access-list fields: %+v", rpcTx)
	}

	_, dynamicMsg := mustSignedMsg(t, &ethtypes.DynamicFeeTx{
		ChainID:    chainID,
		Nonce:      3,
		To:         &to,
		Value:      big.NewInt(13),
		Gas:        40000,
		GasFeeCap:  big.NewInt(300),
		GasTipCap:  big.NewInt(25),
		Data:       []byte{0x3},
		AccessList: ethtypes.AccessList{{Address: to}},
	}, ethtypes.LatestSignerForChainID(chainID), key)
	rpcTx, err = NewRPCTransaction(dynamicMsg, blockHash, 23, 4, big.NewInt(280), chainID)
	if err != nil {
		t.Fatalf("NewRPCTransaction dynamic-fee returned error: %v", err)
	}
	if rpcTx.GasFeeCap == nil || rpcTx.GasTipCap == nil || rpcTx.GasPrice.ToInt().Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("unexpected dynamic-fee gas pricing: %+v", rpcTx)
	}

	_, dynamicMsg = mustSignedMsg(t, &ethtypes.DynamicFeeTx{
		ChainID:    chainID,
		Nonce:      4,
		To:         &to,
		Value:      big.NewInt(14),
		Gas:        45000,
		GasFeeCap:  big.NewInt(500),
		GasTipCap:  big.NewInt(25),
		Data:       []byte{0x4},
		AccessList: ethtypes.AccessList{{Address: to}},
	}, ethtypes.LatestSignerForChainID(chainID), key)
	rpcTx, err = NewRPCTransaction(dynamicMsg, blockHash, 24, 5, big.NewInt(100), chainID)
	if err != nil {
		t.Fatalf("NewRPCTransaction dynamic-fee returned error: %v", err)
	}
	if rpcTx.GasPrice.ToInt().Cmp(big.NewInt(125)) != 0 {
		t.Fatalf("unexpected effective gas price: %s", rpcTx.GasPrice.ToInt())
	}

	unsigned := evmtypes.NewTx(chainID, 5, &to, big.NewInt(1), 21000, big.NewInt(1), nil, nil, nil, nil)
	if _, err := NewRPCTransaction(unsigned, common.Hash{}, 0, 0, nil, chainID); err == nil {
		t.Fatal("expected sender recovery error for unsigned tx")
	}
}

func TestMiscRPCUtils(t *testing.T) {
	if BaseFeeFromEvents([]abci.Event{{Type: "ignored"}}) != nil {
		t.Fatal("base fee parsing is stubbed and should return nil")
	}

	if err := CheckTxFee(big.NewInt(params.Ether), 1, 2); err != nil {
		t.Fatalf("expected fee under cap to pass: %v", err)
	}
	if err := CheckTxFee(big.NewInt(params.Ether), 2, 1); err == nil {
		t.Fatal("expected fee cap error")
	}
	if err := CheckTxFee(big.NewInt(params.Ether), math.MaxUint16, 0); err != nil {
		t.Fatalf("expected zero cap to disable checks: %v", err)
	}

	res := &abci.ExecTxResult{Code: 1, Log: "prefix " + ExceedBlockGasLimitError + " 123"}
	if !TxExceedBlockGasLimit(res) {
		t.Fatal("expected exceed block gas limit match")
	}
	if !TxSuccessOrExceedsBlockGasLimit(res) {
		t.Fatal("expected success-or-exceed helper to accept block gas limit errors")
	}
	if TxSuccessOrExceedsBlockGasLimit(&abci.ExecTxResult{Code: 2, Log: "other"}) {
		t.Fatal("unexpected success-or-exceed result")
	}
}

func mustSignedMsg(t *testing.T, txData ethtypes.TxData, signer ethtypes.Signer, key *ecdsa.PrivateKey) (*ethtypes.Transaction, *evmtypes.MsgEthereumTx) {
	t.Helper()
	tx := ethtypes.NewTx(txData)
	signed, err := ethtypes.SignTx(tx, signer, key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	msg := &evmtypes.MsgEthereumTx{}
	if err := msg.FromSignedEthereumTx(signed, signer); err != nil {
		t.Fatalf("FromSignedEthereumTx: %v", err)
	}
	return signed, msg
}
