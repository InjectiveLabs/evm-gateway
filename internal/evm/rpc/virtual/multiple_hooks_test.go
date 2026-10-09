package virtual

import (
	"math/big"
	"strconv"
	"testing"

	"github.com/cometbft/cometbft/abci/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	virtualbank "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/bank"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

func TestSyntheticTxsKeepBankAggregationWithZeroOneOrMultipleHooks(t *testing.T) {
	for _, hookCount := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(hookCount), func(t *testing.T) {
			events := []types.Event{bankTransferEvent("")}
			if hookCount > 0 {
				events = append(events, multiHookEvent(t, "2", true, 70))
			}
			events = append(events, bankReceivedEvent("4"))
			if hookCount > 1 {
				events = append(events, multiHookEvent(t, "5", false, 40))
			}
			events = append(events, bankTransferEvent("5"))
			response, err := ParseResponse(&types.ExecTxResult{GasUsed: 500, Events: events})
			require.NoError(t, err)
			ctx := multiHookContext()
			txs, err := response.SyntheticTxs(ctx)
			require.NoError(t, err)

			require.Len(t, txs, 1+hookCount)
			bankTx := txs[0]
			require.Equal(t, CosmosTxHash(ctx.Tx), bankTx.Transaction.Hash)
			require.Nil(t, bankTx.Transaction.CosmosMsgIndex)
			require.Equal(t, virtualbank.ContractAddress, *bankTx.Transaction.To)
			require.Len(t, bankTx.Receipt.Logs, 3)
			bankGas := uint64(500)
			if hookCount > 0 {
				bankGas -= 70
			}
			if hookCount > 1 {
				bankGas -= 40
			}
			require.EqualValues(t, bankGas, bankTx.Receipt.GasUsed)
			require.EqualValues(t, 100+bankGas, bankTx.Receipt.CumulativeGasUsed)
			for i, topic := range []common.Hash{virtualbank.TopicTransfer, virtualbank.TopicCoinReceived, virtualbank.TopicTransfer} {
				require.Equal(t, topic, bankTx.Receipt.Logs[i].Topics[0])
				require.Nil(t, bankTx.Receipt.Logs[i].CosmosMsgIndex)
			}
			assertMultiHookMetadata(t, ctx, txs)
			if hookCount == 0 {
				return
			}
			require.Equal(t, crypto.Keccak256Hash(ctx.Tx.Hash(), []byte("@2")), txs[1].Transaction.Hash)
			require.EqualValues(t, 2, *txs[1].Transaction.CosmosMsgIndex)
			require.EqualValues(t, ethtypes.ReceiptStatusSuccessful, txs[1].Receipt.Status)
			require.Len(t, txs[1].Receipt.Logs, 2)
			require.EqualValues(t, 170+bankGas, txs[1].Receipt.CumulativeGasUsed)
			if hookCount == 1 {
				return
			}
			require.Equal(t, crypto.Keccak256Hash(ctx.Tx.Hash(), []byte("@5")), txs[2].Transaction.Hash)
			require.EqualValues(t, 5, *txs[2].Transaction.CosmosMsgIndex)
			require.EqualValues(t, ethtypes.ReceiptStatusFailed, txs[2].Receipt.Status)
			require.Equal(t, "execution reverted", txs[2].Receipt.VMError)
			require.Len(t, txs[2].Receipt.Logs, 1)
			require.EqualValues(t, 560, txs[1].Receipt.CumulativeGasUsed)
			require.EqualValues(t, 600, txs[2].Receipt.CumulativeGasUsed)
			assertMultiHookMetadata(t, ctx, txs)
		})
	}
}

func TestSyntheticTxsMultipleHooksWithoutBankEventsUseMessageOrder(t *testing.T) {
	ctx := multiHookContext()
	ctx.TotalMessages = 13
	response, err := ParseResponse(&types.ExecTxResult{GasUsed: 500, Events: []types.Event{
		multiHookEvent(t, "12", true, 40),
		multiHookEvent(t, "2", true, 70),
	}})
	require.NoError(t, err)
	txs, err := response.SyntheticTxs(ctx)
	require.NoError(t, err)
	require.Len(t, txs, 2)
	require.Equal(t, crypto.Keccak256Hash(ctx.Tx.Hash(), []byte("@2")), txs[0].Transaction.Hash)
	require.Equal(t, crypto.Keccak256Hash(ctx.Tx.Hash(), []byte("@12")), txs[1].Transaction.Hash)
	require.EqualValues(t, 170, txs[0].Receipt.CumulativeGasUsed)
	require.EqualValues(t, 210, txs[1].Receipt.CumulativeGasUsed)
	assertMultiHookMetadata(t, ctx, txs)
}

func TestSyntheticTxsRejectAmbiguousMultipleHookIdentities(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"duplicate", "2", "2"},
		{"out_of_range", "2", "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := ParseResponse(&types.ExecTxResult{Events: []types.Event{
				multiHookEvent(t, tc.first, true, 70),
				multiHookEvent(t, tc.second, true, 40),
			}})
			require.NoError(t, err)
			txs, err := response.SyntheticTxs(multiHookContext())
			require.ErrorContains(t, err, "msg_index")
			require.Empty(t, txs)
		})
	}
}

func TestSyntheticTxsRejectOutOfRangeSingleHook(t *testing.T) {
	response, err := ParseResponse(&types.ExecTxResult{Events: []types.Event{
		multiHookEvent(t, "7", true, 70),
	}})
	require.NoError(t, err)
	txs, err := response.SyntheticTxs(multiHookContext())
	require.ErrorContains(t, err, "msg_index")
	require.Empty(t, txs)
}

func TestParseResponseRejectsHookWithoutMessageIndex(t *testing.T) {
	_, err := ParseResponse(&types.ExecTxResult{Events: []types.Event{
		multiHookEvent(t, "", true, 70),
	}})
	require.ErrorContains(t, err, "msg_index")
}

func TestSyntheticTxsDoNotSplitHooksBelongingToGenuineEthereumMessages(t *testing.T) {
	ctx := multiHookContext()
	ctx.EthereumMessageIndexes = map[int]bool{2: true}
	response, err := ParseResponse(&types.ExecTxResult{Events: []types.Event{
		multiHookEvent(t, "2", true, 70),
		multiHookEvent(t, "5", true, 40),
		bankTransferEvent("2"),
		bankTransferEvent("4"),
	}})
	require.NoError(t, err)
	txs, err := response.SyntheticTxs(ctx)
	require.NoError(t, err)
	require.Len(t, txs, 2)
	require.Equal(t, CosmosTxHash(ctx.Tx), txs[0].Transaction.Hash)
	require.Nil(t, txs[0].Transaction.CosmosMsgIndex)
	require.Len(t, txs[0].Receipt.Logs, 1)
	require.Equal(t, IBCHookTxHash(ctx.Tx, 5), txs[1].Transaction.Hash)
	require.EqualValues(t, 5, *txs[1].Transaction.CosmosMsgIndex)
	require.Len(t, txs[1].Receipt.Logs, 2)
	logs := response.LogsForMessage(2, ctx.TotalMessages, LogContext{})
	require.Len(t, logs, 3)
	require.EqualValues(t, 2, *logs[0].CosmosMsgIndex)
	require.Nil(t, logs[2].CosmosMsgIndex)
}

func TestSyntheticTxsSplitGasDoesNotRepeatGenuineEthereumGas(t *testing.T) {
	ctx := multiHookContext()
	ctx.EthereumGasUsed = 80
	response, err := ParseResponse(&types.ExecTxResult{GasUsed: 500, Events: []types.Event{
		bankTransferEvent("4"),
		multiHookEvent(t, "2", true, 70),
		multiHookEvent(t, "5", false, 40),
	}})
	require.NoError(t, err)
	txs, err := response.SyntheticTxs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 310, txs[0].Receipt.GasUsed)
	require.EqualValues(t, 490, txs[0].Receipt.CumulativeGasUsed)
	require.EqualValues(t, 600, txs[2].Receipt.CumulativeGasUsed)
}

func multiHookContext() TxContext {
	return TxContext{
		Tx: cmtypes.Tx("multiple packet Cosmos transaction"), TotalMessages: 7,
		BlockHash: common.HexToHash("0xbeef"), BlockNumber: 12,
		TxIndex: 4, FirstLogIndex: 6, CumulativeGasUsedBefore: 100,
		ChainID: big.NewInt(1776),
	}
}

func multiHookEvent(t *testing.T, index string, success bool, gas uint64) types.Event {
	t.Helper()
	hook := &evmtypes.EventIBCHookCall{
		DestinationPort: "transfer", DestinationChannel: "channel-9", Sequence: gas,
		Contract: "0x1111111111111111111111111111111111111111",
		Input:    []byte{byte(gas)}, Success: success, GasUsed: gas,
	}
	if success {
		hook.Logs = []*evmtypes.Log{{
			Address: "0x2222222222222222222222222222222222222222",
			Topics:  []string{common.HexToHash("0x1234").Hex()}, Data: []byte{byte(gas)},
		}}
	} else {
		hook.Error = "execution reverted"
	}
	event := mustTypedEvent(t, hook)
	if index != "" {
		event.Attributes = append(event.Attributes, types.EventAttribute{Key: "msg_index", Value: index})
	}
	return event
}

func assertMultiHookMetadata(t *testing.T, ctx TxContext, txs []*Tx) {
	t.Helper()
	logIndex := ctx.FirstLogIndex
	cosmosHash := OriginalCosmosTxHash(ctx.Tx)
	for i, tx := range txs {
		require.EqualValues(t, ctx.TxIndex+uint64(i), *tx.Transaction.TransactionIndex)
		require.EqualValues(t, ctx.TxIndex+uint64(i), tx.Receipt.TransactionIndex)
		require.Equal(t, tx.Transaction.Hash, tx.Receipt.TransactionHash)
		require.Equal(t, cosmosHash, *tx.Transaction.CosmosHash)
		for _, log := range tx.Receipt.Logs {
			require.Equal(t, tx.Transaction.Hash, log.TxHash)
			require.EqualValues(t, tx.Receipt.TransactionIndex, log.TxIndex)
			require.EqualValues(t, logIndex, log.Index)
			require.Equal(t, cosmosHash, *log.CosmosHash)
			require.Equal(t, tx.Transaction.CosmosMsgIndex, log.CosmosMsgIndex)
			require.True(t, log.Virtual)
			logIndex++
		}
	}
}
