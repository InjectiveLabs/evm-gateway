package backend

import (
	"fmt"
	"strings"
	"testing"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/syncstatus"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

// Block 185519530 holds, in order, the Ethereum txs:
//
//	cosmos tx 4: msgBefore (succeeded)
//	cosmos tx 5: TxMultiMsgReverted (message 0, failed), TxMultiMsgUnexecuted (message 1, never executed)
//	cosmos tx 7: msgAfter (succeeded)
const multiMsgCosmosTx = 5

func multiMsgFixtureHashes(t *testing.T) (before, after common.Hash) {
	t.Helper()
	all := msgHashes(fixtureEthMsgs(t, mainnetfx.HeightMultiMsgFailure))
	if len(all) != 4 || all[1] != mainnetfx.TxMultiMsgReverted || all[2] != mainnetfx.TxMultiMsgUnexecuted {
		t.Fatalf("unexpected fixture msgs %v", all)
	}
	return all[0], all[3]
}

// withMultiMsgLog serves block results of HeightMultiMsgFailure whose failed
// Cosmos tx reports log instead of the mainnet one.
func withMultiMsgLog(t *testing.T, comet *fixtureComet, log string) {
	t.Helper()
	results := *comet.results[mainnetfx.HeightMultiMsgFailure]
	results.TxResults = append([]*abci.ExecTxResult(nil), results.TxResults...)
	failed := *results.TxResults[multiMsgCosmosTx]
	failed.Log = log
	results.TxResults[multiMsgCosmosTx] = &failed
	comet.results[mainnetfx.HeightMultiMsgFailure] = &results
}

func traceTxResponse(tag string) *evmtypes.QueryTraceTxResponse {
	return &evmtypes.QueryTraceTxResponse{Data: []byte(fmt.Sprintf(`{"type":"CALL","tag":%q}`, tag))}
}

func traceBlockMatcher(height int64, txs []common.Hash) interface{} {
	return mock.MatchedBy(func(req *evmtypes.QueryTraceBlockRequest) bool {
		return req.BlockNumber == height && hashesEqual(msgHashes(req.Txs), txs)
	})
}

// assertFailedFrame checks that got is the synthetic failed callTracer frame
// of the visible tx hash, built from its tx and receipt as b serves them.
func assertFailedFrame(t *testing.T, b *Backend, label string, hash common.Hash, got interface{}, err error, wantErr string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	frame := frameMap(t, got)
	if errMsg, _ := frame["error"].(string); !strings.Contains(errMsg, wantErr) {
		t.Fatalf("%s: expected frame error containing %q, got %v", label, wantErr, frame)
	}
	tx, txErr := b.GetTransactionByHash(hash)
	receipt, receiptErr := b.GetTransactionReceipt(hash)
	if txErr != nil || receiptErr != nil || tx == nil || receipt == nil {
		t.Fatalf("%s: tx %v %v receipt %v %v", label, tx, txErr, receipt, receiptErr)
	}
	want := frameMap(t, map[string]interface{}{
		"from": tx.From, "to": tx.To, "gas": tx.Gas, "gasUsed": receipt["gasUsed"], "input": tx.Input, "value": tx.Value,
	})
	for key, value := range want {
		if frame[key] != value {
			t.Fatalf("%s: frame %s = %v, want %v (frame %v)", label, key, frame[key], value, frame)
		}
	}
	if frame["type"] != "CALL" || frame["calls"] != nil {
		t.Fatalf("%s: unexpected frame %v", label, frame)
	}
}

// TestTraceTransactionMultiMsgFailure covers the mainnet report: message 0 of
// the Cosmos tx reverted and message 1 never ran. Message 0 replays and
// reproduces its failure, message 1 gets a synthetic failed frame (a mock call
// would panic on any unexpected TraceTx), and later txs do not replay
// message 1.
func TestTraceTransactionMultiMsgFailure(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)

	query := &rpcmocks.EVMQueryClient{}
	query.On("TraceTx", mock.Anything, traceTxMatcher(height, mainnetfx.TxMultiMsgReverted, []common.Hash{before})).
		Return(traceTxResponse("reverted"), nil).Once()
	query.On("TraceTx", mock.Anything, traceTxMatcher(height, after, []common.Hash{before, mainnetfx.TxMultiMsgReverted})).
		Return(traceTxResponse("after"), nil).Once()

	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t), query: query})
	for hash, want := range map[common.Hash]string{mainnetfx.TxMultiMsgReverted: "reverted", after: "after"} {
		got, err := b.TraceTransaction(hash, callTracerConfig)
		if err != nil {
			t.Fatalf("TraceTransaction(%s): %v", hash.Hex(), err)
		}
		if traceTag(t, got) != want {
			t.Fatalf("TraceTransaction(%s): unexpected frame %v", hash.Hex(), got)
		}
	}

	got, err := b.TraceTransaction(mainnetfx.TxMultiMsgUnexecuted, callTracerConfig)
	assertFailedFrame(t, b, "unexecuted message", mainnetfx.TxMultiMsgUnexecuted, got, err, "not executed: message 0 of the cosmos transaction failed before it")
	if cached, err := kv.GetTraceTransaction(mainnetfx.TxMultiMsgUnexecuted, callTracerConfig); err == nil {
		t.Fatalf("synthetic frame must not be cached as a tx trace: %s", cached)
	}
	query.AssertExpectations(t)
}

// TestTraceBlockMultiMsgFailure replays the block without the message that
// never ran and gives it a synthetic failed frame, also from the cached block
// trace.
func TestTraceBlockMultiMsgFailure(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprintf("virtual=%v", virtual), func(t *testing.T) {
			kv, _ := newFixtureKV(t, virtual)
			indexFixtureBlocks(t, kv, height)

			query := &rpcmocks.EVMQueryClient{}
			query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, []common.Hash{before, mainnetfx.TxMultiMsgReverted, after})).
				Return(traceBlockResponse(t, 3), nil).Once()

			b := newFixtureBackend(t, fixtureBackendOptions{virtual: virtual, indexer: kv, comet: newFixtureComet(t), query: query})
			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			if err != nil {
				t.Fatalf("TraceBlock: %v", err)
			}
			query.AssertExpectations(t)

			visible, err := kv.GetRPCTransactionHashesByBlockHeight(height)
			if err != nil {
				t.Fatalf("visible hashes: %v", err)
			}
			assertHashList(t, "trace hashes", traceHashes(got), visible)
			byHash := make(map[common.Hash]*rpctypes.TxTraceResult)
			for _, entry := range got {
				byHash[entry.TxHash] = entry
			}
			for hash, tag := range map[common.Hash]string{before: "a", mainnetfx.TxMultiMsgReverted: "b", after: "c"} {
				if entry := byHash[hash]; entry == nil || entry.Error != "" || traceTag(t, entry.Result) != tag {
					t.Fatalf("entry %s: %#v want tag %s", hash.Hex(), entry, tag)
				}
			}
			skipped := byHash[mainnetfx.TxMultiMsgUnexecuted]
			if skipped == nil || skipped.Error != "" {
				t.Fatalf("unexecuted entry: %#v", skipped)
			}
			assertFailedFrame(t, b, "unexecuted entry", mainnetfx.TxMultiMsgUnexecuted, skipped.Result, nil, "not executed: message 0")

			// served from the cached block trace, without the chain
			cached := newFixtureBackend(t, fixtureBackendOptions{virtual: virtual, indexer: kv, comet: newFixtureComet(t), query: &rpcmocks.EVMQueryClient{}})
			again, err := cached.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			if err != nil {
				t.Fatalf("cached TraceBlock: %v", err)
			}
			assertHashList(t, "cached trace hashes", traceHashes(again), visible)
			trace, err := cached.TraceTransaction(mainnetfx.TxMultiMsgUnexecuted, callTracerConfig)
			assertFailedFrame(t, cached, "cached unexecuted message", mainnetfx.TxMultiMsgUnexecuted, trace, err, "not executed: message 0")
			trace, err = cached.TraceTransaction(after, callTracerConfig)
			if err != nil || traceTag(t, trace) != "c" {
				t.Fatalf("cached TraceTransaction(after): %v %v", trace, err)
			}
		})
	}
}

// TestTraceMultiMsgFailureOfLaterMessage covers a Cosmos tx whose second
// message failed: the first one executed and was reverted with it. The first
// gets a synthetic failed frame, the second is replayed alone after the first
// (by debug_traceTransaction and for its block entry), and no other trace
// replays either of them.
func TestTraceMultiMsgFailureOfLaterMessage(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	first, second := mainnetfx.TxMultiMsgReverted, mainnetfx.TxMultiMsgUnexecuted
	log := `failed to execute message; message index: 1: {"tx_hash":"` + second.Hex() + `","gas_used":21000,"vm_error":"execution reverted"}`

	t.Run("transaction", func(t *testing.T) {
		kv, _ := newFixtureKV(t, false)
		indexFixtureBlocks(t, kv, height)
		comet := newFixtureComet(t)
		withMultiMsgLog(t, comet, log)

		query := &rpcmocks.EVMQueryClient{}
		query.On("TraceTx", mock.Anything, traceTxMatcher(height, second, []common.Hash{before, first})).
			Return(traceTxResponse("second"), nil).Once()
		query.On("TraceTx", mock.Anything, traceTxMatcher(height, after, []common.Hash{before})).
			Return(traceTxResponse("after"), nil).Once()

		b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
		got, err := b.TraceTransaction(first, callTracerConfig)
		assertFailedFrame(t, b, "reverted message", first, got, err, "reverted: message 1 of the cosmos transaction failed")
		for hash, want := range map[common.Hash]string{second: "second", after: "after"} {
			got, err := b.TraceTransaction(hash, callTracerConfig)
			if err != nil || traceTag(t, got) != want {
				t.Fatalf("TraceTransaction(%s): %v %v", hash.Hex(), got, err)
			}
		}
		query.AssertExpectations(t)
	})

	t.Run("block", func(t *testing.T) {
		kv, _ := newFixtureKV(t, false)
		indexFixtureBlocks(t, kv, height)
		comet := newFixtureComet(t)
		withMultiMsgLog(t, comet, log)

		query := &rpcmocks.EVMQueryClient{}
		query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, []common.Hash{before, after})).
			Return(traceBlockResponse(t, 2), nil).Once()
		query.On("TraceTx", mock.Anything, traceTxMatcher(height, second, []common.Hash{before, first})).
			Return(traceTxResponse("second"), nil).Once()

		b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
		got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
		if err != nil {
			t.Fatalf("TraceBlock: %v", err)
		}
		query.AssertExpectations(t)
		assertHashList(t, "trace hashes", traceHashes(got), []common.Hash{before, first, second, after})
		if traceTag(t, got[0].Result) != "a" || traceTag(t, got[3].Result) != "b" {
			t.Fatalf("unexpected frames: %#v", got)
		}
		if got[1].Error != "" {
			t.Fatalf("reverted entry: %#v", got[1])
		}
		assertFailedFrame(t, b, "reverted entry", first, got[1].Result, nil, "reverted: message 1")
		// replayed alone, on top of the reverted message, like debug_traceTransaction
		if got[2].Error != "" || traceTag(t, got[2].Result) != "second" {
			t.Fatalf("failed entry: %#v", got[2])
		}
		// cached as its tx trace, like debug_traceTransaction caches it
		cached, err := kv.GetTraceTransaction(second, callTracerConfig)
		require.NoError(t, err)
		require.Contains(t, string(cached), "second")
	})

	t.Run("block after transaction", func(t *testing.T) {
		kv, _ := newFixtureKV(t, false)
		indexFixtureBlocks(t, kv, height)
		comet := newFixtureComet(t)
		withMultiMsgLog(t, comet, log)

		// the alone message replays once: the block trace reuses its tx trace
		query := &rpcmocks.EVMQueryClient{}
		query.On("TraceTx", mock.Anything, traceTxMatcher(height, second, []common.Hash{before, first})).
			Return(traceTxResponse("second"), nil).Once()
		query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, []common.Hash{before, after})).
			Return(traceBlockResponse(t, 2), nil).Once()

		b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
		_, err := b.TraceTransaction(second, callTracerConfig)
		require.NoError(t, err)
		got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
		require.NoError(t, err)
		query.AssertExpectations(t)
		require.Empty(t, got[2].Error)
		require.Equal(t, "second", traceTag(t, got[2].Result))
	})
}

// TestTraceMultiMsgFailureWithoutMessageIndex covers a failed Cosmos tx with
// several messages whose log names no message: none of them is replayed, all
// get a synthetic failed frame.
func TestTraceMultiMsgFailureWithoutMessageIndex(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	comet := newFixtureComet(t)
	withMultiMsgLog(t, comet, "recovered: panic during message execution")

	query := &rpcmocks.EVMQueryClient{}
	query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, []common.Hash{before, after})).
		Return(traceBlockResponse(t, 2), nil).Once()
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})

	for _, hash := range []common.Hash{mainnetfx.TxMultiMsgReverted, mainnetfx.TxMultiMsgUnexecuted} {
		got, err := b.TraceTransaction(hash, callTracerConfig)
		assertFailedFrame(t, b, hash.Hex(), hash, got, err, "the failed message is unknown")
	}
	got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	if err != nil {
		t.Fatalf("TraceBlock: %v", err)
	}
	query.AssertExpectations(t)
	if len(got) != 4 || traceTag(t, got[0].Result) != "a" || traceTag(t, got[3].Result) != "b" {
		t.Fatalf("unexpected entries: %#v", got)
	}
	for _, entry := range got[1:3] {
		if entry.Error != "" {
			t.Fatalf("unexpected entry-level error: %#v", entry)
		}
		assertFailedFrame(t, b, entry.TxHash.Hex(), entry.TxHash, entry.Result, nil, "the failed message is unknown")
	}
}

func TestMsgTraceError(t *testing.T) {
	failedAt := func(log string) *abci.ExecTxResult {
		return &abci.ExecTxResult{Code: 3, Codespace: evmtypes.ModuleName, Log: log, Events: []abci.Event{{Type: "fee"}}}
	}
	indexed := failedAt("failed to execute message; message index: 1: {}")
	unnamed := failedAt("out of gas")
	anteFailed := &abci.ExecTxResult{Code: 5, Codespace: "sdk", Log: "insufficient funds"}

	for _, tc := range []struct {
		name     string
		res      *abci.ExecTxResult
		msgIndex int
		msgCount int
		want     string
	}{
		{"no result", nil, 0, 2, ""},
		{"success", &abci.ExecTxResult{}, 1, 2, ""},
		{"ante failed", anteFailed, 0, 1, ""},
		{"before failed", indexed, 0, 3, "reverted: message 1 of the cosmos transaction failed"},
		{"failed", indexed, 1, 3, ""},
		{"after failed", indexed, 2, 3, "not executed: message 1 of the cosmos transaction failed before it"},
		{"single unnamed", unnamed, 0, 1, ""},
		{"several unnamed", unnamed, 0, 2, "the failed message is unknown"},
	} {
		got := msgTraceError(tc.res, tc.msgIndex, tc.msgCount)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestTraceBlockLiveMultiMsgFailureFromFetchedData traces the multi-message
// failure block before it is indexed, without virtualization: the synthetic
// frames and the alone message's trace come from the fetched block and block
// results, with no tx lookup by hash (PR #26 review). The trace makes one Block
// and one BlockResults request, also when the indexer is caught up and its miss
// is authoritative.
func TestTraceBlockLiveMultiMsgFailureFromFetchedData(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	first, second := mainnetfx.TxMultiMsgReverted, mainnetfx.TxMultiMsgUnexecuted
	laterLog := `failed to execute message; message index: 1: {"tx_hash":"` + second.Hex() + `","gas_used":21000,"vm_error":"execution reverted"}`

	for _, tc := range []struct {
		name    string
		log     string
		replay  []common.Hash
		failed  map[common.Hash]string
		aloneOn common.Hash
	}{
		{name: "unexecuted message", replay: []common.Hash{before, first, after},
			failed: map[common.Hash]string{second: "not executed: message 0"}},
		{name: "reverted and alone message", log: laterLog, replay: []common.Hash{before, after},
			failed: map[common.Hash]string{first: "reverted: message 1"}, aloneOn: second},
	} {
		for _, indexed := range []string{"no indexer", "caught-up empty indexer"} {
			t.Run(tc.name+"/"+indexed, func(t *testing.T) {
				comet := newFixtureComet(t)
				if tc.log != "" {
					withMultiMsgLog(t, comet, tc.log)
				}
				query := liveQuery()
				query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, tc.replay)).
					Return(traceBlockResponse(t, len(tc.replay)), nil).Once()
				if tc.aloneOn != (common.Hash{}) {
					query.On("TraceTx", mock.Anything, traceTxMatcher(height, tc.aloneOn, []common.Hash{before, first})).
						Return(traceTxResponse("alone"), nil).Once()
				}
				opts := fixtureBackendOptions{comet: comet, query: query}
				if indexed != "no indexer" {
					opts.indexer, _ = newFixtureKV(t, false)
				}
				b := newFixtureBackend(t, opts)
				if indexed != "no indexer" {
					tracker := syncstatus.NewTracker(1, 1)
					tracker.SetPhase(syncstatus.PhaseForwardSync)
					tracker.SetHistoryCovered(true)
					tracker.MarkBlock(height-1, true)
					b.syncStatus = tracker
					require.True(t, b.indexerCaughtUp())
				}

				got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
				require.NoError(t, err)
				query.AssertExpectations(t)
				require.Equal(t, map[string]int{"Block": 1, "BlockResults": 1}, comet.calls)
				assertHashList(t, "trace hashes", traceHashes(got), []common.Hash{before, first, second, after})

				// the frames match the live block txs and receipts
				block, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), true)
				require.NoError(t, err)
				receipts, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: ptrBlockNumber(height)})
				require.NoError(t, err)
				for i, entry := range got {
					require.Empty(t, entry.Error, "entry %s", entry.TxHash.Hex())
					wantErr, synthetic := tc.failed[entry.TxHash]
					switch {
					case synthetic:
						tx := block["transactions"].([]interface{})[i].(*rpctypes.RPCTransaction)
						frame := frameMap(t, entry.Result)
						require.Contains(t, frame["error"], wantErr)
						require.Equal(t, frameMap(t, map[string]interface{}{"from": tx.From, "to": tx.To, "gas": tx.Gas, "gasUsed": receipts[i]["gasUsed"], "input": tx.Input, "value": tx.Value}),
							frameMap(t, map[string]interface{}{"from": frame["from"], "to": frame["to"], "gas": frame["gas"], "gasUsed": frame["gasUsed"], "input": frame["input"], "value": frame["value"]}))
					case entry.TxHash == tc.aloneOn:
						require.Equal(t, "alone", traceTag(t, entry.Result))
					default:
						require.NotEmpty(t, traceTag(t, entry.Result))
					}
				}
			})
		}
	}
}

// TestTraceTransactionUnreplayableFromFetchedData: debug_traceTransaction of a
// message that never ran builds its frame from the block data it fetched to
// locate the message, with no further request, when the block is not indexed.
func TestTraceTransactionUnreplayableFromFetchedData(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	hash := mainnetfx.TxMultiMsgUnexecuted
	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprintf("virtual=%v", virtual), func(t *testing.T) {
			indexed, _ := newFixtureKV(t, virtual)
			indexFixtureBlocks(t, indexed, height)
			located, err := indexed.GetByTxHash(hash)
			require.NoError(t, err)

			// the tx is located (as by the Comet tx index) but its block is not indexed
			empty, _ := newFixtureKV(t, virtual)
			idx := &overrideIndexer{TxIndexer: empty, txs: map[common.Hash]*chaintypes.TxResult{hash: located}}
			comet := newFixtureComet(t)
			b := newFixtureBackend(t, fixtureBackendOptions{virtual: virtual, indexer: idx, comet: comet, query: liveQuery()})

			got, err := b.TraceTransaction(hash, callTracerConfig)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"Block": 1, "BlockResults": 1}, comet.calls)
			frame := frameMap(t, got)
			require.Contains(t, frame["error"], "not executed: message 0")
			require.Equal(t, "CALL", frame["type"])
			require.Equal(t, "0x5208", frame["gasUsed"])
		})
	}
}

func ptrBlockNumber(height int64) *rpctypes.BlockNumber {
	n := rpctypes.BlockNumber(height)
	return &n
}
