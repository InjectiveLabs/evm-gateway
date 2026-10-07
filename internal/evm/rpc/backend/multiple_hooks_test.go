package backend

import (
	"encoding/json"
	"math/big"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	channeltypes "github.com/cosmos/ibc-go/v8/modules/core/04-channel/types"
	ibccoretypes "github.com/cosmos/ibc-go/v8/modules/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
	virtualbank "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/bank"
	virtualibc "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual/ibc"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
)

// Exercise client discovery, cached transactions/receipts, live reconstruction,
// and reindex cleanup with one or two packets in a Cosmos transaction.
func TestMultipleHooksQueryableAndConsistentWithLiveBlock(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		withBank, withNative bool
		hookCount            int
	}{
		{name: "single_hook", hookCount: 1},
		{name: "single_hook_and_bank", hookCount: 1, withBank: true},
		{name: "hooks_only", hookCount: 2},
		{name: "hooks_and_bank", hookCount: 2, withBank: true},
		{name: "native_evm_hooks_and_bank", hookCount: 2, withBank: true, withNative: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded := backendReceiptTestSDKTx{msgs: []sdk.Msg{
				&banktypes.MsgSend{}, &channeltypes.MsgRecvPacket{},
				&banktypes.MsgSend{}, &channeltypes.MsgRecvPacket{},
			}}
			nativeGas := int64(0)
			if tc.withNative {
				decoded.msgs[0] = signedDynamicFeeMsg(t, big.NewInt(1337), big.NewInt(2), big.NewInt(1))
				decoded.extensionOptions = backendReceiptTestEVMExtensionOptions()
				nativeGas = 21000
			}
			clientCtx := client.Context{TxConfig: backendTraceTestTxConfig{decoder: func(raw []byte) (sdk.Tx, error) {
				if string(raw) == "two packets" {
					return decoded, nil
				}
				return backendReceiptTestSDKTx{msgs: []sdk.Msg{&banktypes.MsgSend{}, &banktypes.MsgSend{}, &banktypes.MsgSend{}}}, nil
			}}}
			db := dbm.NewMemDB()
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			kv := indexer.NewKVIndexer(db, backendTestLogger(), clientCtx, indexer.WithVirtualCosmosEvents(true, "1337"))
			block := cmtypes.MakeBlock(12, []cmtypes.Tx{cmtypes.Tx("two packets"), cmtypes.Tx("following bank tx")}, &cmtypes.Commit{Height: 11}, nil)
			block.Header.ValidatorsHash = common.HexToHash("0x1234").Bytes()
			block.Header.AppHash = common.HexToHash("0xabcd").Bytes()
			events := []abci.Event{backendMultiHookEvent(t, "1", true, 70)}
			if tc.withBank {
				events = append([]abci.Event{backendMultiBankEvent("")}, events...)
				events = append(events, backendMultiBankEvent("2"))
			}
			if tc.hookCount > 1 {
				events = append(events, backendMultiHookEvent(t, "3", false, 40))
			}
			if tc.withBank {
				events = append(events, backendMultiBankEvent("3"))
			}
			results := &cmrpctypes.ResultBlockResults{
				Height: block.Height,
				TxResults: []*abci.ExecTxResult{
					{GasUsed: 500 + nativeGas, Events: events},
					{GasUsed: 40, Events: []abci.Event{backendMultiBankEvent("2")}},
				},
				FinalizeBlockEvents: []abci.Event{backendTestBaseFeeEvent(big.NewInt(1))},
			}
			if tc.withNative {
				msg := decoded.msgs[0].(*evmtypes.MsgEthereumTx)
				results.TxResults[0].Events = append([]abci.Event{ethereumTxEvent(msg.Hash(), 0, uint64(nativeGas))}, events...)
				results.TxResults[0].Data = mustMarshalBackendTxMsgData(t, &evmtypes.MsgEthereumTxResponse{Hash: msg.Hash().Hex(), GasUsed: uint64(nativeGas)})
			}
			require.NoError(t, kv.IndexBlockWithResults(block, results))
			b := NewBackend(backendTestLogger(), appconfig.Config{
				ChainID: "injective-1", EVMChainID: "1337", OfflineRPCOnly: true, VirtualizeCosmosEvents: true,
			}, clientCtx, clientCtx, false, kv, nil)
			view, err := b.liveVirtualBlockView(&cmrpctypes.ResultBlock{Block: block}, results)
			require.NoError(t, err)
			hashes, err := kv.GetRPCTransactionHashesByBlockHeight(block.Height)
			require.NoError(t, err)
			count := 1 + tc.hookCount
			bankIndex := 0
			if tc.withNative {
				count++
				bankIndex++
			}
			if tc.withBank {
				count++
				require.Equal(t, virtual.CosmosTxHash(block.Txs[0]), hashes[bankIndex])
			}
			require.Len(t, hashes, count)
			require.Len(t, view.Transactions, count)
			for i, hash := range hashes {
				tx, err := b.GetTransactionByHash(hash)
				require.NoError(t, err)
				require.Equal(t, view.Transactions[i], tx)
				receipt, err := b.GetTransactionReceipt(hash)
				require.NoError(t, err)
				assertMultiHookJSONEqual(t, view.Receipts[i], receipt)
				byIndex, err := b.GetTransactionByBlockNumberAndIndex(rpctypes.BlockNumber(block.Height), hexutil.Uint(i))
				require.NoError(t, err)
				require.Equal(t, tx, byIndex)
			}
			meta, err := kv.GetBlockMetaByHeight(block.Height)
			require.NoError(t, err)
			require.EqualValues(t, count, meta.EthTxCount)
			require.EqualValues(t, 540+nativeGas, meta.GasUsed)
			blockNumber := rpctypes.BlockNumber(block.Height)
			receipts, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber})
			require.NoError(t, err)
			assertMultiHookJSONEqual(t, view.Receipts, receipts)

			// This is the existing eth_getLogs address/topic query used by clients.
			summaries, err := b.GetFilteredLogs(common.BytesToHash(block.Hash()),
				[]common.Address{virtualibc.ContractAddress}, [][]common.Hash{{virtualibc.TopicHookCall}})
			require.NoError(t, err)
			require.Len(t, summaries, tc.hookCount)
			cosmosHash := virtual.OriginalCosmosTxHash(block.Txs[0])
			for i, msgIndex := range []int{1, 3}[:tc.hookCount] {
				require.Equal(t, cosmosHash, *summaries[i].CosmosHash)
				require.EqualValues(t, msgIndex, *summaries[i].CosmosMsgIndex)
				require.Len(t, summaries[i].Topics, 3)
				require.EqualValues(t, msgIndex, summaries[i].Topics[2].Big().Uint64())
				hash := crypto.Keccak256Hash(cosmosHash.Bytes(), []byte("@"), []byte{byte('0' + msgIndex)})
				require.Equal(t, hash, summaries[i].TxHash)
				receipt, err := b.GetTransactionReceipt(summaries[i].TxHash)
				require.NoError(t, err)
				require.EqualValues(t, 1-i, receipt["status"])
			}
			cachedLogs, err := kv.GetLogsByBlockHeight(block.Height)
			require.NoError(t, err)
			require.Len(t, cachedLogs, len(view.Logs))
			for i, logs := range view.Logs {
				require.Len(t, cachedLogs[i], len(logs))
				if len(logs) > 0 {
					require.Equal(t, logs, cachedLogs[i])
				}
			}
			if tc.withBank {
				require.Len(t, cachedLogs[bankIndex], 3)
				for _, log := range cachedLogs[bankIndex] {
					require.Nil(t, log.CosmosMsgIndex)
					require.Equal(t, virtualbank.TopicTransfer, log.Topics[0])
				}
			}

			// A repeated index must not duplicate results, and deleting the block
			// must also remove the new per-message hook records.
			require.NoError(t, kv.IndexBlockWithResults(block, results))
			reindexed, err := kv.GetRPCTransactionHashesByBlockHeight(block.Height)
			require.NoError(t, err)
			require.Equal(t, hashes, reindexed)
			require.NoError(t, kv.DeleteBlock(block.Height))
			for _, hash := range hashes {
				_, err := kv.GetRPCTransactionByHash(hash)
				require.Error(t, err)
				_, err = kv.GetReceiptByTxHash(hash)
				require.Error(t, err)
			}
		})
	}
}

func assertMultiHookJSONEqual(t *testing.T, expected, actual interface{}) {
	t.Helper()
	expectedJSON, err := json.Marshal(expected)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(actual)
	require.NoError(t, err)
	require.JSONEq(t, string(expectedJSON), string(actualJSON))
}

func backendMultiBankEvent(msgIndex string) abci.Event {
	attrs := []abci.EventAttribute{
		{Key: "sender", Value: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Key: "recipient", Value: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Key: "amount", Value: "5inj"},
	}
	if msgIndex != "" {
		attrs = append(attrs, abci.EventAttribute{Key: "msg_index", Value: msgIndex})
	}
	return abci.Event{Type: virtualbank.EventTypeTransfer, Attributes: attrs}
}

func backendMultiHookEvent(t *testing.T, msgIndex string, success bool, gas uint64) abci.Event {
	t.Helper()
	hook := &evmtypes.EventIBCHookCall{
		DestinationPort: "transfer", DestinationChannel: "channel-9", Sequence: gas,
		Contract: "0x1111111111111111111111111111111111111111", Input: []byte{byte(gas)},
		Success: success, GasUsed: gas,
	}
	if success {
		hook.Logs = []*evmtypes.Log{{
			Address: "0x2222222222222222222222222222222222222222",
			Topics:  []string{common.HexToHash("0x1234").Hex()}, Data: []byte{0xaa},
		}}
	} else {
		hook.Error = "execution reverted"
	}
	typed, err := sdk.TypedEventToEvent(hook)
	require.NoError(t, err)
	event := sdk.Events{typed}.ToABCIEvents()[0]
	if !success {
		event.Type = ibccoretypes.ErrorAttributeKeyPrefix + event.Type
		for i := range event.Attributes {
			event.Attributes[i].Key = ibccoretypes.ErrorAttributeKeyPrefix + event.Attributes[i].Key
		}
	}
	event.Attributes = append(event.Attributes, abci.EventAttribute{Key: "msg_index", Value: msgIndex})
	return event
}
