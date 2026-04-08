package backend

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestTraceChainIDPrefersEthereumTxChainID(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	_, msg := mustSignedTraceMsg(
		t,
		&ethtypes.DynamicFeeTx{
			ChainID:   big.NewInt(1776),
			Nonce:     1,
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(2),
			Gas:       21000,
			To:        ptrAddress(common.HexToAddress("0x0000000000000000000000000000000000000001")),
			Value:     big.NewInt(0),
			Data:      []byte{0x1},
		},
		ethtypes.LatestSignerForChainID(big.NewInt(1776)),
		key,
	)

	if got := traceChainID(big.NewInt(1), msg); got != 1776 {
		t.Fatalf("unexpected trace chain id: got %d want 1776", got)
	}
}

func TestTraceChainIDFallsBackWhenTxChainIDUnavailable(t *testing.T) {
	if got := traceChainID(big.NewInt(1), &evmtypes.MsgEthereumTx{}); got != 1 {
		t.Fatalf("unexpected fallback chain id: got %d want 1", got)
	}
}

func mustSignedTraceMsg(t *testing.T, txData ethtypes.TxData, signer ethtypes.Signer, key *ecdsa.PrivateKey) (*ethtypes.Transaction, *evmtypes.MsgEthereumTx) {
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

func ptrAddress(v common.Address) *common.Address {
	return &v
}
