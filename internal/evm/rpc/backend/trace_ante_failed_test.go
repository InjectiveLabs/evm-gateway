package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

var callTracerConfig = &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: "callTracer"}}

func traceTag(t *testing.T, result interface{}) string {
	t.Helper()
	frame, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected trace frame type %T: %#v", result, result)
	}
	tag, _ := frame["tag"].(string)
	return tag
}

// TestTraceTransactionFromCachedBlockLooksUpByHash is the bd53603 regression:
// the aligned block trace follows the visible tx order (virtual txs included)
// while EthTxIndex only counts Ethereum txs, so a positional lookup returned
// the virtual `type: 0` placeholder or the previous tx's frame.
func TestTraceTransactionFromCachedBlockLooksUpByHash(t *testing.T) {
	const height = 100
	beginBlock := common.HexToHash("0xbb")
	txA := common.HexToHash("0xaa")
	txB := common.HexToHash("0xcc")
	txErr := common.HexToHash("0xee")
	txGone := common.HexToHash("0xff")

	trace, err := json.Marshal([]*rpctypes.TxTraceResult{
		{TxHash: beginBlock, Result: map[string]interface{}{"type": 0}},
		{TxHash: txA, Result: map[string]interface{}{"type": "CALL", "tag": "A"}},
		{TxHash: txB, Result: map[string]interface{}{"type": "CALL", "tag": "B"}},
		{TxHash: txErr, Error: "execution timeout"},
	})
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	idx := &overrideIndexer{
		txs: map[common.Hash]*chaintypes.TxResult{
			txA:    {Height: height, EthTxIndex: 0},
			txB:    {Height: height, EthTxIndex: 1},
			txErr:  {Height: height, EthTxIndex: 2},
			txGone: {Height: height, EthTxIndex: 3},
		},
		traceBlock: trace,
		visible:    []common.Hash{beginBlock, txA, txB, txErr},
		virtual:    map[common.Hash]bool{beginBlock: true},
	}
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: idx})

	for hash, want := range map[common.Hash]string{txA: "A", txB: "B"} {
		got, err := b.TraceTransaction(hash, callTracerConfig)
		if err != nil {
			t.Fatalf("TraceTransaction(%s): %v", hash.Hex(), err)
		}
		if tag := traceTag(t, got); tag != want {
			t.Fatalf("TraceTransaction(%s): got frame %v want tag %s", hash.Hex(), got, want)
		}
	}

	if _, err := b.traceTransactionFromCachedBlock(txErr, callTracerConfig); err == nil || err.Error() != "execution timeout" {
		t.Fatalf("expected cached trace error entry, got %v", err)
	}
	if _, err := b.traceTransactionFromCachedBlock(txGone, callTracerConfig); err == nil || err.Error() != "transaction trace not found in cached block trace" {
		t.Fatalf("expected missing trace error, got %v", err)
	}
	// offline: no live fallback once the cached derivation fails
	if _, err := b.TraceTransaction(txGone, callTracerConfig); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("expected offline trace error, got %v", err)
	}
}

func TestTraceTransactionFromCachedBlockErrors(t *testing.T) {
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true})
	if _, err := b.traceTransactionFromCachedBlock(common.HexToHash("0x01"), nil); err == nil || err.Error() != "trace cache unavailable" {
		t.Fatalf("expected trace cache unavailable, got %v", err)
	}

	idx := &overrideIndexer{traceBlockErr: errFixtureUnavailable}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: idx})
	if _, err := b.traceTransactionFromCachedBlock(common.HexToHash("0x01"), nil); err == nil {
		t.Fatalf("expected tx lookup error")
	}
	idx.txs = map[common.Hash]*chaintypes.TxResult{common.HexToHash("0x01"): {Height: 7}}
	if _, err := b.traceTransactionFromCachedBlock(common.HexToHash("0x01"), nil); !errors.Is(err, errFixtureUnavailable) {
		t.Fatalf("expected block trace error, got %v", err)
	}
	idx.traceBlockErr = nil
	idx.traceBlock = json.RawMessage(`{`)
	if _, err := b.traceTransactionFromCachedBlock(common.HexToHash("0x01"), nil); err == nil {
		t.Fatalf("expected decode error")
	}
	// an entry without hash needs the block, which offline mode can't fetch
	idx.traceBlock = json.RawMessage(`[{"result":{"type":"CALL"}}]`)
	if _, err := b.traceTransactionFromCachedBlock(common.HexToHash("0x01"), nil); err == nil || !strings.Contains(err.Error(), "populating trace transaction hashes") {
		t.Fatalf("expected populate error, got %v", err)
	}
}

func TestAlignTraceBlockResultsDropsHiddenAndFillsMissing(t *testing.T) {
	virtualTx := common.HexToHash("0x01")
	ethTx := common.HexToHash("0x02")
	missingEth := common.HexToHash("0x03")
	anteFailed := common.HexToHash("0x04")

	ethResult := &rpctypes.TxTraceResult{TxHash: ethTx, Result: map[string]interface{}{"type": "CALL", "tag": "first"}}
	duplicate := &rpctypes.TxTraceResult{TxHash: ethTx, Result: map[string]interface{}{"type": "CALL", "tag": "dup"}}
	hidden := &rpctypes.TxTraceResult{TxHash: anteFailed, Error: "insufficient balance for transfer"}

	aligned := alignTraceBlockResults(
		[]*rpctypes.TxTraceResult{hidden, ethResult, duplicate},
		[]common.Hash{virtualTx, ethTx, missingEth},
		func(hash common.Hash) bool { return hash == virtualTx },
	)
	if len(aligned) != 3 {
		t.Fatalf("unexpected aligned count %d", len(aligned))
	}
	if result, ok := aligned[0].Result.(map[string]interface{}); !ok || aligned[0].TxHash != virtualTx || result["type"] != 0 {
		t.Fatalf("virtual tx must get an empty trace: %#v", aligned[0])
	}
	if aligned[1] != ethResult {
		t.Fatalf("duplicate result hashes must keep the first entry: %#v", aligned[1])
	}
	if aligned[2].TxHash != missingEth || aligned[2].Error != traceUnavailableError || aligned[2].Result != nil {
		t.Fatalf("missing ethereum tx must get an error entry: %#v", aligned[2])
	}
	for _, entry := range aligned {
		if entry.TxHash == anteFailed {
			t.Fatalf("trace of a hidden tx leaked: %#v", entry)
		}
	}

	// nil isVirtual: every missing tx is an Ethereum tx
	aligned = alignTraceBlockResults(nil, []common.Hash{virtualTx}, nil)
	if len(aligned) != 1 || aligned[0].Error != traceUnavailableError {
		t.Fatalf("nil isVirtual: %#v", aligned)
	}

	passthrough := []*rpctypes.TxTraceResult{hidden}
	if got := alignTraceBlockResults(passthrough, nil, nil); len(got) != 1 || got[0] != hidden {
		t.Fatalf("empty visible list must pass results through: %#v", got)
	}
	withNil := []*rpctypes.TxTraceResult{nil, ethResult}
	if got := alignTraceBlockResults(withNil, []common.Hash{ethTx}, nil); len(got) != 2 || got[0] != nil {
		t.Fatalf("nil result must pass results through: %#v", got)
	}
	noHash := []*rpctypes.TxTraceResult{{Result: map[string]interface{}{"type": "CALL"}}}
	if got := alignTraceBlockResults(noHash, []common.Hash{ethTx}, nil); len(got) != 1 || got[0] != noHash[0] {
		t.Fatalf("result without hash must pass results through: %#v", got)
	}
}

func TestAlignTraceBlockResultsWithVisibleTransactionsIndexerPaths(t *testing.T) {
	ethTx := common.HexToHash("0x02")
	missing := common.HexToHash("0x03")
	results := []*rpctypes.TxTraceResult{{TxHash: ethTx, Result: map[string]interface{}{"type": "CALL"}}}

	b := newFixtureBackend(t, fixtureBackendOptions{offline: true})
	if got := b.alignTraceBlockResultsWithVisibleTransactions(results, 1); len(got) != 1 || got[0] != results[0] {
		t.Fatalf("nil indexer must pass results through: %#v", got)
	}

	idx := &overrideIndexer{visibleErr: errFixtureUnavailable}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: idx})
	if got := b.alignTraceBlockResultsWithVisibleTransactions(results, 1); len(got) != 1 || got[0] != results[0] {
		t.Fatalf("visible lookup error must pass results through: %#v", got)
	}

	idx = &overrideIndexer{visible: []common.Hash{ethTx, missing}, virtualErr: errFixtureUnavailable}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: idx})
	got := b.alignTraceBlockResultsWithVisibleTransactions(results, 1)
	if len(got) != 2 || got[1].Error != traceUnavailableError {
		t.Fatalf("virtual lookup error must be treated as an ethereum tx: %#v", got)
	}

	// indexer without virtual tx lookup: every missing tx is an Ethereum tx
	plain := &plainTraceIndexer{TxIndexer: &overrideIndexer{visible: []common.Hash{ethTx, missing}}}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: plain})
	got = b.alignTraceBlockResultsWithVisibleTransactions(results, 1)
	if len(got) != 2 || got[1].Error != traceUnavailableError {
		t.Fatalf("indexer without virtual lookup: %#v", got)
	}

	// real KV indexer: the ante-failed entries of the block are dropped
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, mainnetfx.HeightEx1Failed)
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: kv})
	all := make([]*rpctypes.TxTraceResult, 0)
	for _, hash := range msgHashes(fixtureEthMsgs(t, mainnetfx.HeightEx1Failed)) {
		all = append(all, &rpctypes.TxTraceResult{TxHash: hash, Result: map[string]interface{}{"type": "CALL"}})
	}
	got = b.alignTraceBlockResultsWithVisibleTransactions(all, mainnetfx.HeightEx1Failed)
	assertHashList(t, "aligned", traceHashes(got), mainnetfx.VisibleTxs[mainnetfx.HeightEx1Failed])

	// virtualized KV indexer: virtual txs have no trace and get empty frames
	virtualKV, _ := newFixtureKV(t, true)
	indexFixtureBlocks(t, virtualKV, mainnetfx.HeightEx1Failed)
	visible, err := virtualKV.GetRPCTransactionHashesByBlockHeight(mainnetfx.HeightEx1Failed)
	if err != nil {
		t.Fatalf("visible hashes: %v", err)
	}
	if len(visible) <= len(mainnetfx.VisibleTxs[mainnetfx.HeightEx1Failed]) {
		t.Fatalf("expected virtual txs in the visible list, got %d", len(visible))
	}
	b = newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: virtualKV, virtual: true})
	got = b.alignTraceBlockResultsWithVisibleTransactions(all, mainnetfx.HeightEx1Failed)
	assertHashList(t, "virtual aligned", traceHashes(got), visible)
	ethereum := make(map[common.Hash]bool)
	for _, hash := range mainnetfx.VisibleTxs[mainnetfx.HeightEx1Failed] {
		ethereum[hash] = true
	}
	for _, entry := range got {
		frame, ok := entry.Result.(map[string]interface{})
		if !ok || entry.Error != "" {
			t.Fatalf("unexpected entry %#v", entry)
		}
		if ethereum[entry.TxHash] != (frame["type"] == "CALL") || (!ethereum[entry.TxHash] && frame["type"] != 0) {
			t.Fatalf("unexpected frame for %s: %#v", entry.TxHash.Hex(), frame)
		}
	}
}

func traceHashes(results []*rpctypes.TxTraceResult) []common.Hash {
	hashes := make([]common.Hash, 0, len(results))
	for _, result := range results {
		hashes = append(hashes, result.TxHash)
	}
	return hashes
}

// TestTraceBlockCachedLegacyTraces serves block traces cached by the pre-fix
// deployment: with hashes and ante-failed error entries (bd53603 format), and
// without hashes (raw chain format), which replayed every Ethereum message.
func TestTraceBlockCachedLegacyTraces(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	sentry, err := mainnetfx.SentryTrace(height)
	if err != nil {
		t.Fatalf("SentryTrace: %v", err)
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(sentry, &entries); err != nil {
		t.Fatalf("decode sentry trace: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("unexpected sentry trace entries %d", len(entries))
	}
	stripped := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		copied := make(map[string]interface{}, len(entry))
		for k, v := range entry {
			if k != "txHash" {
				copied[k] = v
			}
		}
		stripped = append(stripped, copied)
	}
	strippedRaw, err := json.Marshal(stripped)
	if err != nil {
		t.Fatalf("marshal stripped: %v", err)
	}

	wantFrom := []string{
		"0x108421084210bc1cf3caf8a6486403a2edb2d3cd",
		"0x62238746c50b3285603c4525d4a2f7be7eef3f4d",
	}
	for name, raw := range map[string]json.RawMessage{"with hashes": sentry, "without hashes": strippedRaw} {
		t.Run(name, func(t *testing.T) {
			kv, _ := newFixtureKV(t, false)
			indexFixtureBlocks(t, kv, height)
			if err := kv.SetTraceBlockByHeight(height, callTracerConfig, raw); err != nil {
				t.Fatalf("SetTraceBlockByHeight: %v", err)
			}
			b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t)})

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			if err != nil {
				t.Fatalf("TraceBlock: %v", err)
			}
			assertHashList(t, "trace hashes", traceHashes(got), mainnetfx.VisibleTxs[height])
			for i, entry := range got {
				frame, ok := entry.Result.(map[string]interface{})
				if entry.Error != "" || !ok || frame["type"] != "CALL" || frame["from"] != wantFrom[i] {
					t.Fatalf("entry %d: unexpected trace %#v", i, entry)
				}
			}

			// debug_traceTransaction derives the same frames from the cached block
			for i, hash := range mainnetfx.VisibleTxs[height] {
				frame, err := b.TraceTransaction(hash, callTracerConfig)
				if err != nil {
					t.Fatalf("TraceTransaction(%s): %v", hash.Hex(), err)
				}
				if frame.(map[string]interface{})["from"] != wantFrom[i] {
					t.Fatalf("TraceTransaction(%s): wrong frame %v", hash.Hex(), frame)
				}
			}
		})
	}
}

func TestPopulateTraceBlockTransactionHashesErrors(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	noHashes := func(n int) []*rpctypes.TxTraceResult {
		out := make([]*rpctypes.TxTraceResult, n)
		for i := range out {
			out[i] = &rpctypes.TxTraceResult{Result: map[string]interface{}{"type": "CALL"}}
		}
		return out
	}

	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{comet: comet})
	// 4 Ethereum messages in the block: a 3-entry legacy trace can't be matched
	if err := b.populateTraceBlockTransactionHashes(noHashes(3), height, nil); err == nil || !strings.Contains(err.Error(), "trace result count 3 does not match Ethereum transaction count 4") {
		t.Fatalf("expected count mismatch, got %v", err)
	}
	if err := b.populateTraceBlockTransactionHashes(nil, height, nil); err != nil {
		t.Fatalf("empty results: %v", err)
	}
	withNil := append(noHashes(3), nil)
	if err := b.populateTraceBlockTransactionHashes(withNil, height, nil); err != nil {
		t.Fatalf("nil entry: %v", err)
	}
	if withNil[3] == nil || withNil[3].TxHash != mainnetfx.VisibleTxs[height][1] {
		t.Fatalf("nil entry must be replaced and hashed: %#v", withNil[3])
	}

	comet.blockErr = errFixtureUnavailable
	if err := b.populateTraceBlockTransactionHashes(noHashes(4), height, nil); err == nil || !strings.Contains(err.Error(), "block not found while populating") {
		t.Fatalf("expected block error, got %v", err)
	}
	comet.blockErr = nil
	comet.nilBlock = true
	if err := b.populateTraceBlockTransactionHashes(noHashes(4), height, nil); err == nil || err.Error() != "block not found while populating trace transaction hashes" {
		t.Fatalf("expected missing block error, got %v", err)
	}
}

func traceBlockResponse(t *testing.T, n int) *evmtypes.QueryTraceBlockResponse {
	t.Helper()
	results := make([]map[string]interface{}, n)
	for i := range results {
		results[i] = map[string]interface{}{"result": map[string]interface{}{"type": "CALL", "tag": string(rune('a' + i))}}
	}
	bz, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &evmtypes.QueryTraceBlockResponse{Data: bz}
}

// TestTraceBlockLiveSkipsAnteFailedTxs replays only the txs that were
// executed on chain and aligns the results to the visible tx list.
func TestTraceBlockLiveSkipsAnteFailedTxs(t *testing.T) {
	for _, height := range []int64{mainnetfx.HeightEx1Failed, mainnetfx.HeightEx2Failed} {
		height := height
		t.Run("height", func(t *testing.T) {
			kv, _ := newFixtureKV(t, false)
			indexFixtureBlocks(t, kv, height)
			visible := mainnetfx.VisibleTxs[height]

			query := &rpcmocks.EVMQueryClient{}
			query.On("TraceBlock", mock.Anything, mock.MatchedBy(func(req *evmtypes.QueryTraceBlockRequest) bool {
				return req.BlockNumber == height && hashesEqual(msgHashes(req.Txs), visible)
			})).Return(traceBlockResponse(t, len(visible)), nil).Once()

			b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t), query: query})
			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			if err != nil {
				t.Fatalf("TraceBlock: %v", err)
			}
			assertHashList(t, "trace hashes", traceHashes(got), visible)
			for i, entry := range got {
				if traceTag(t, entry.Result) != string(rune('a'+i)) {
					t.Fatalf("entry %d misaligned: %#v", i, entry)
				}
			}
			query.AssertExpectations(t)

			cached, err := kv.GetTraceBlockByHeight(height, callTracerConfig)
			if err != nil {
				t.Fatalf("GetTraceBlockByHeight: %v", err)
			}
			decoded, err := decodeCachedTraceBlock(cached)
			if err != nil {
				t.Fatalf("decode cached trace: %v", err)
			}
			assertHashList(t, "cached trace hashes", traceHashes(decoded), visible)
		})
	}
}

func TestTraceBlockLiveWithoutBlockResultsReplaysAllMessages(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	all := msgHashes(fixtureEthMsgs(t, height))
	if len(all) != 4 {
		t.Fatalf("unexpected fixture msg count %d", len(all))
	}

	query := &rpcmocks.EVMQueryClient{}
	query.On("TraceBlock", mock.Anything, mock.MatchedBy(func(req *evmtypes.QueryTraceBlockRequest) bool {
		return hashesEqual(msgHashes(req.Txs), all)
	})).Return(traceBlockResponse(t, len(all)), nil).Once()

	comet := newFixtureComet(t)
	comet.resultsErr = errFixtureUnavailable
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
	got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	if err != nil {
		t.Fatalf("TraceBlock: %v", err)
	}
	// the hidden txs' results are still dropped by the alignment
	assertHashList(t, "trace hashes", traceHashes(got), mainnetfx.VisibleTxs[height])
	if traceTag(t, got[0].Result) != "a" || traceTag(t, got[1].Result) != "d" {
		t.Fatalf("unexpected aligned frames: %#v", got)
	}
	query.AssertExpectations(t)

	// nil block results without error
	comet.resultsErr = nil
	delete(comet.results, height)
	if txResults := b.blockTxResultsForTrace(height); txResults != nil {
		t.Fatalf("expected nil tx results")
	}
}

func TestTraceBlockLiveBlockWithoutEthereumTxs(t *testing.T) {
	kv, _ := newFixtureKV(t, false)
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t)})
	block := &cmrpctypes.ResultBlock{Block: tmtypes.MakeBlock(42, nil, nil, nil)}
	got, err := b.TraceBlock(rpctypes.BlockNumber(42), callTracerConfig, block)
	if err != nil {
		t.Fatalf("TraceBlock: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("expected empty trace, got %#v", got)
	}
	cached, err := kv.GetTraceBlockByHeight(42, callTracerConfig)
	if err != nil || string(cached) != "[]" {
		t.Fatalf("expected cached empty trace, got %q %v", cached, err)
	}
}

func traceTxMatcher(height int64, target common.Hash, predecessors []common.Hash) interface{} {
	return mock.MatchedBy(func(req *evmtypes.QueryTraceTxRequest) bool {
		return req.BlockNumber == height && req.Msg.Hash() == target && hashesEqual(msgHashes(req.Predecessors), predecessors)
	})
}

// TestTraceTransactionLiveSkipsAnteFailedPredecessors replays only the
// predecessors that were executed on chain.
func TestTraceTransactionLiveSkipsAnteFailedPredecessors(t *testing.T) {
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, mainnetfx.HeightEx1Included, mainnetfx.HeightEx2Failed)
	ex2Visible := mainnetfx.VisibleTxs[mainnetfx.HeightEx2Failed]

	query := &rpcmocks.EVMQueryClient{}
	query.On("TraceTx", mock.Anything, traceTxMatcher(mainnetfx.HeightEx1Included, mainnetfx.TxEx1, nil)).
		Return(&evmtypes.QueryTraceTxResponse{Data: []byte(`{"type":"CALL","tag":"ex1"}`)}, nil).Once()
	// cosmos tx 14 follows the ante-failed cosmos txs 10-13
	query.On("TraceTx", mock.Anything, traceTxMatcher(mainnetfx.HeightEx2Failed, ex2Visible[1], ex2Visible[:1])).
		Return(&evmtypes.QueryTraceTxResponse{Data: []byte(`{"type":"CALL","tag":"ex2"}`)}, nil).Once()

	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t), query: query})
	for hash, want := range map[common.Hash]string{mainnetfx.TxEx1: "ex1", ex2Visible[1]: "ex2"} {
		got, err := b.TraceTransaction(hash, callTracerConfig)
		if err != nil {
			t.Fatalf("TraceTransaction(%s): %v", hash.Hex(), err)
		}
		if traceTag(t, got) != want {
			t.Fatalf("TraceTransaction(%s): unexpected frame %v", hash.Hex(), got)
		}
		cached, err := kv.GetTraceTransaction(hash, callTracerConfig)
		if err != nil || !strings.Contains(string(cached), want) {
			t.Fatalf("trace not cached for %s: %q %v", hash.Hex(), cached, err)
		}
	}
	query.AssertExpectations(t)
}

// TestTraceTransactionLiveRejectsAnteFailedTx traces a hash that legacy state
// still maps to its ante-failed inclusion.
func TestTraceTransactionLiveRejectsAnteFailedTx(t *testing.T) {
	kv, _ := newLegacyFixtureKV(t)
	res, err := kv.GetByTxHash(mainnetfx.TxEx2)
	if err != nil || res.Height != mainnetfx.HeightEx2Failed || res.TxIndex != 12 {
		t.Fatalf("legacy state must map TxEx2 to its ante-failed inclusion: %#v %v", res, err)
	}
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t)})
	_, err = b.TraceTransaction(mainnetfx.TxEx2, callTracerConfig)
	if err == nil || !strings.Contains(err.Error(), "failed in the ante handler") {
		t.Fatalf("expected ante-failed error, got %v", err)
	}
}

func TestTraceTransactionLiveTxIndexOutOfBounds(t *testing.T) {
	block := mustFixtureResultBlock(t, mainnetfx.HeightEx2Included)
	hash := common.HexToHash("0x0b")
	inner, _ := newFixtureKV(t, false)
	idx := &overrideIndexer{
		TxIndexer: inner,
		txs: map[common.Hash]*chaintypes.TxResult{
			hash: {Height: mainnetfx.HeightEx2Included, TxIndex: uint32(len(block.Block.Txs))},
		},
	}
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: idx, comet: newFixtureComet(t)})
	_, err := b.TraceTransaction(hash, callTracerConfig)
	if err == nil || !strings.Contains(err.Error(), "transaction not included in block") {
		t.Fatalf("expected out of bounds error, got %v", err)
	}
}

var _ txindexer.TxIndexer = (*overrideIndexer)(nil)

// plainTraceIndexer hides the virtual tx lookup of the wrapped indexer.
type plainTraceIndexer struct {
	txindexer.TxIndexer
}

func (p *plainTraceIndexer) WithContext(context.Context) txindexer.TxIndexer { return p }
