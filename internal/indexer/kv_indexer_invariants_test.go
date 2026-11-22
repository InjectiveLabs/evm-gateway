package indexer

import (
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

// TestIndexedReceiptsAreSelfConsistent checks, on every mainnet fixture block
// and in both virtualization modes, that every Ethereum tx listed for a block
// resolves by hash to the same block and index, and that its receipt logs
// carry the receipt's tx hash and index. Non-virtualized logs take their
// transactionIndex from NormalizeTxResponseIndexes, so this pins its tx
// counting to the indexer's visibility rule.
func TestIndexedReceiptsAreSelfConsistent(t *testing.T) {
	for _, virtual := range []bool{false, true} {
		for _, height := range mainnetfx.AllHeights {
			db := dbm.NewMemDB()
			kv := newFixtureIndexer(t, db, WithVirtualCosmosEvents(virtual, "1776"))
			indexFixture(t, kv, height)
			assertBlockSelfConsistent(t, kv, height, virtual)
		}
	}
}

func assertBlockSelfConsistent(t *testing.T, kv *KVIndexer, height int64, virtual bool) {
	t.Helper()
	hashes, err := kv.GetRPCTransactionHashesByBlockHeight(height)
	if err != nil {
		t.Fatalf("virtual=%v height %d: hashes: %v", virtual, height, err)
	}
	var nextLogIndex uint
	for i, hash := range hashes {
		rpcTx, err := kv.GetRPCTransactionByHash(hash)
		if err != nil || rpcTx == nil {
			t.Fatalf("virtual=%v height %d tx %d %s: by hash: %v", virtual, height, i, hash.Hex(), err)
		}
		if rpcTx.BlockNumber == nil || rpcTx.BlockNumber.ToInt().Int64() != height ||
			rpcTx.TransactionIndex == nil || uint64(*rpcTx.TransactionIndex) != uint64(i) {
			t.Fatalf("virtual=%v height %d tx %d %s: by hash points at block %v index %v",
				virtual, height, i, hash.Hex(), rpcTx.BlockNumber, rpcTx.TransactionIndex)
		}
		receipt, err := kv.GetReceiptByTxHash(hash)
		if err != nil {
			t.Fatalf("virtual=%v height %d tx %d %s: receipt: %v", virtual, height, i, hash.Hex(), err)
		}
		if got := receipt["transactionIndex"].(hexutil.Uint64); uint64(got) != uint64(i) {
			t.Fatalf("virtual=%v height %d tx %s: receipt index %d want %d", virtual, height, hash.Hex(), got, i)
		}
		if got := receipt["transactionHash"].(common.Hash); got != hash {
			t.Fatalf("virtual=%v height %d tx %d: receipt hash %s want %s", virtual, height, i, got.Hex(), hash.Hex())
		}
		for _, log := range receiptLogs(t, receipt) {
			if uint64(log.TxIndex) != uint64(i) || log.TxHash != hash {
				t.Fatalf("virtual=%v height %d tx %d %s: log %d has tx index %d hash %s",
					virtual, height, i, hash.Hex(), log.Index, log.TxIndex, log.TxHash.Hex())
			}
			if uint(log.Index) != nextLogIndex {
				t.Fatalf("virtual=%v height %d tx %d %s: log index %d want %d",
					virtual, height, i, hash.Hex(), log.Index, nextLogIndex)
			}
			nextLogIndex++
		}
	}
}

func receiptLogs(t *testing.T, receipt map[string]interface{}) []*virtual.RPCLog {
	t.Helper()
	logs, ok := receipt["logs"].([]*virtual.RPCLog)
	if !ok {
		t.Fatalf("receipt logs have type %T", receipt["logs"])
	}
	return logs
}
