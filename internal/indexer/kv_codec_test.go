package indexer

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

// TestKVCapnpBlockLogsRoundTrip verifies grouped block logs preserve virtual
// metadata through the Cap'n Proto cache codec.
func TestKVCapnpBlockLogsRoundTrip(t *testing.T) {
	cosmosHash := common.HexToHash("0xabc")
	logs := [][]*virtualbank.RPCLog{{
		{
			Address:     virtualbank.ContractAddress,
			Topics:      []common.Hash{virtualbank.TopicTransfer, common.HexToHash("0x01")},
			Data:        []byte{0x01, 0x02, 0x03},
			BlockNumber: 12,
			TxHash:      common.HexToHash("0x1234"),
			TxIndex:     3,
			BlockHash:   common.HexToHash("0x5678"),
			Index:       4,
			Virtual:     true,
			CosmosHash:  &cosmosHash,
		},
	}}

	bz := mustMarshalBlockLogs(logs)
	require.True(t, bytes.HasPrefix(bz, kvCapnpMagic))

	got, err := unmarshalBlockLogsPayload(bz)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0], 1)
	require.Equal(t, logs[0][0].Address, got[0][0].Address)
	require.Equal(t, logs[0][0].Topics, got[0][0].Topics)
	require.Equal(t, logs[0][0].Data, got[0][0].Data)
	require.Equal(t, logs[0][0].BlockNumber, got[0][0].BlockNumber)
	require.Equal(t, logs[0][0].TxHash, got[0][0].TxHash)
	require.Equal(t, logs[0][0].TxIndex, got[0][0].TxIndex)
	require.Equal(t, logs[0][0].BlockHash, got[0][0].BlockHash)
	require.Equal(t, logs[0][0].Index, got[0][0].Index)
	require.True(t, got[0][0].Virtual)
	require.NotNil(t, got[0][0].CosmosHash)
	require.Equal(t, cosmosHash, *got[0][0].CosmosHash)
}

// TestKVCapnpReceiptRoundTripPreservesOptionalZeroBig verifies optional receipt
// fields survive Cap'n Proto encoding, including zero-valued fee quantities.
func TestKVCapnpReceiptRoundTripPreservesOptionalZeroBig(t *testing.T) {
	to := virtualbank.ContractAddress.Hex()
	reason := ""
	receipt := CachedReceipt{
		Status:            1,
		CumulativeGasUsed: 2,
		GasUsed:           3,
		Reason:            &reason,
		LogsBloom:         hexutil.Encode(make([]byte, 256)),
		Logs: []*virtualbank.RPCLog{{
			Address: virtualbank.ContractAddress,
			Data:    []byte("payload"),
		}},
		TransactionHash:   common.HexToHash("0x1").Hex(),
		BlockHash:         common.HexToHash("0x2").Hex(),
		BlockNumber:       4,
		TransactionIndex:  5,
		EffectiveGasPrice: "0x0",
		From:              common.Address{}.Hex(),
		To:                &to,
		Type:              uint64(ethtypes.LegacyTxType),
	}

	got, err := unmarshalReceiptPayload(mustMarshalReceipt(receipt))
	require.NoError(t, err)
	require.Equal(t, receipt.Status, got.Status)
	require.NotNil(t, got.Reason)
	require.Equal(t, "", *got.Reason)
	require.Equal(t, receipt.LogsBloom, got.LogsBloom)
	require.Equal(t, receipt.EffectiveGasPrice, got.EffectiveGasPrice)
	require.NotNil(t, got.To)
	require.Equal(t, to, *got.To)
	require.Len(t, got.Logs, 1)
	require.Equal(t, hexutil.Bytes("payload"), got.Logs[0].Data)
}

// TestKVCapnpRPCTransactionRoundTrip verifies cached RPC transactions preserve
// optional EIP-2718 and virtual metadata fields.
func TestKVCapnpRPCTransactionRoundTrip(t *testing.T) {
	blockHash := common.HexToHash("0xb1")
	blockNumber := hexBig(99)
	to := common.HexToAddress("0x0000000000000000000000000000000000000800")
	txIndex := hexutil.Uint64(0)
	accesses := ethtypes.AccessList{
		{
			Address:     common.HexToAddress("0x0000000000000000000000000000000000000007"),
			StorageKeys: []common.Hash{common.HexToHash("0x8")},
		},
	}
	cosmosHash := common.HexToHash("0xc0")
	tx := &rpctypes.RPCTransaction{
		BlockHash:        &blockHash,
		BlockNumber:      blockNumber,
		From:             common.HexToAddress("0x0000000000000000000000000000000000000009"),
		Gas:              21000,
		GasPrice:         hexBig(0),
		Hash:             common.HexToHash("0xaa"),
		Input:            hexutil.Bytes{0xde, 0xad},
		Nonce:            1,
		To:               &to,
		TransactionIndex: &txIndex,
		Value:            hexBig(0),
		Type:             hexutil.Uint64(ethtypes.DynamicFeeTxType),
		Accesses:         &accesses,
		ChainID:          hexBig(1439),
		V:                hexBig(1),
		R:                hexBig(2),
		S:                hexBig(3),
		Virtual:          true,
		CosmosHash:       &cosmosHash,
	}

	got, err := unmarshalRPCTransactionPayload(mustMarshalRPCTransaction(tx))
	require.NoError(t, err)
	require.Equal(t, tx.BlockHash, got.BlockHash)
	require.Equal(t, tx.BlockNumber.ToInt(), got.BlockNumber.ToInt())
	require.Equal(t, tx.From, got.From)
	require.Equal(t, tx.Gas, got.Gas)
	require.Equal(t, tx.GasPrice.ToInt(), got.GasPrice.ToInt())
	require.Equal(t, tx.Hash, got.Hash)
	require.Equal(t, tx.Input, got.Input)
	require.Equal(t, tx.To, got.To)
	require.NotNil(t, got.TransactionIndex)
	require.Equal(t, *tx.TransactionIndex, *got.TransactionIndex)
	require.Equal(t, tx.Value.ToInt(), got.Value.ToInt())
	require.Equal(t, *tx.Accesses, *got.Accesses)
	require.Equal(t, tx.ChainID.ToInt(), got.ChainID.ToInt())
	require.True(t, got.Virtual)
	require.NotNil(t, got.CosmosHash)
	require.Equal(t, cosmosHash, *got.CosmosHash)
}

// TestKVCapnpTxResultAndTraceRoundTrip verifies TxResult and trace cache
// payloads round-trip and legacy block metadata still decodes.
func TestKVCapnpTxResultAndTraceRoundTrip(t *testing.T) {
	tx := &chaintypes.TxResult{
		Height:            7,
		TxIndex:           8,
		MsgIndex:          9,
		EthTxIndex:        10,
		Failed:            true,
		GasUsed:           11,
		CumulativeGasUsed: 12,
	}
	gotTx, err := unmarshalTxResultPayload(nil, mustMarshalTxResult(tx))
	require.NoError(t, err)
	require.Equal(t, tx, gotTx)

	raw := []byte(`{"result":{"gas":"0x1"}}`)
	gotRaw, err := unmarshalTracePayload(mustMarshalTracePayload(raw))
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(gotRaw))

	legacyMeta := mustJSON(CachedBlockMeta{Height: 42, Hash: common.HexToHash("0x42").Hex()})
	gotMeta, err := unmarshalBlockMetaPayload(legacyMeta)
	require.NoError(t, err)
	require.Equal(t, int64(42), gotMeta.Height)
}

// hexBig returns a hexutil.Big test value.
func hexBig(v int64) *hexutil.Big {
	b := big.NewInt(v)
	return (*hexutil.Big)(b)
}
