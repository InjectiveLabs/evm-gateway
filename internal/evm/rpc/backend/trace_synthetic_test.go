package backend

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtual"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func tracerConfig(tracer string, tracerCfg string) *rpctypes.TraceConfig {
	cfg := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: tracer}}
	if tracerCfg != "" {
		cfg.TracerConfig = json.RawMessage(tracerCfg)
	}
	return cfg
}

// frameMap returns v as decoded JSON.
func frameMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	bz, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(bz, &out), "trace result %s", bz)
	return out
}

func requireJSON(t *testing.T, want string, got interface{}) {
	t.Helper()
	bz, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, want, string(bz))
}

// liveQuery returns a gRPC mock for live block reads (the miner lookup fails).
func liveQuery() *rpcmocks.EVMQueryClient {
	query := &rpcmocks.EVMQueryClient{}
	query.On("ValidatorAccount", mock.Anything, mock.Anything).Return(nil, errFixtureUnavailable).Maybe()
	return query
}

func syntheticFixtureTx() (*rpctypes.RPCTransaction, map[string]interface{}) {
	to := common.HexToAddress("0x0000000000000000000000000000000000000800")
	tx := &rpctypes.RPCTransaction{
		Hash:    common.HexToHash("0xaa"),
		From:    common.Address{},
		To:      &to,
		Gas:     0,
		Input:   hexutil.Bytes{},
		Value:   (*hexutil.Big)(big.NewInt(0)),
		Virtual: true,
	}
	receipt := map[string]interface{}{
		"status":  hexutil.Uint(1),
		"gasUsed": hexutil.Uint64(70000),
		"logs": []*virtual.RPCLog{{
			Address: to,
			Topics:  []common.Hash{common.HexToHash("0x01"), common.HexToHash("0x02")},
			Data:    []byte{0xbe, 0xef},
		}},
	}
	return tx, receipt
}

// TestSyntheticTraceResult pins the geth shape of synthetic traces for each
// tracer.
func TestSyntheticTraceResult(t *testing.T) {
	tx, receipt := syntheticFixtureTx()
	failed := func(extra map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range receipt {
			out[k] = v
		}
		out["status"] = hexutil.Uint(0)
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	for _, tc := range []struct {
		name    string
		receipt map[string]interface{}
		config  *rpctypes.TraceConfig
		errMsg  string
		want    string
	}{
		{
			name: "callTracer", receipt: receipt, config: callTracerConfig,
			want: `{"type":"CALL","from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000800",
				"gas":"0x0","gasUsed":"0x11170","input":"0x","value":"0x0"}`,
		},
		{
			name: "callTracer withLog", receipt: receipt, config: tracerConfig("callTracer", `{"withLog":true,"onlyTopCall":true}`),
			want: `{"type":"CALL","from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000800",
				"gas":"0x0","gasUsed":"0x11170","input":"0x","value":"0x0",
				"logs":[{"address":"0x0000000000000000000000000000000000000800","topics":[
					"0x0000000000000000000000000000000000000000000000000000000000000001",
					"0x0000000000000000000000000000000000000000000000000000000000000002"],"data":"0xbeef","position":"0x0"}]}`,
		},
		{
			name: "callTracer failed with vm error", receipt: failed(map[string]interface{}{"vmError": "execution reverted", "reason": "nope"}),
			config: tracerConfig("callTracer", `{"withLog":true}`),
			want: `{"type":"CALL","from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000800",
				"gas":"0x0","gasUsed":"0x11170","input":"0x","value":"0x0","error":"execution reverted","revertReason":"nope"}`,
		},
		{
			name: "callTracer failed cosmos tx", receipt: failed(nil), config: callTracerConfig,
			want: `{"type":"CALL","from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000800",
				"gas":"0x0","gasUsed":"0x11170","input":"0x","value":"0x0","error":"cosmos transaction failed"}`,
		},
		{
			name: "callTracer error message", receipt: receipt, config: callTracerConfig, errMsg: "not executed",
			want: `{"type":"CALL","from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000800",
				"gas":"0x0","gasUsed":"0x11170","input":"0x","value":"0x0","error":"not executed"}`,
		},
		{name: "struct logger", receipt: receipt, config: nil, want: `{"gas":70000,"failed":false,"returnValue":"0x","structLogs":[]}`},
		{name: "struct logger failed", receipt: receipt, config: &rpctypes.TraceConfig{}, errMsg: "reverted", want: `{"gas":70000,"failed":true,"returnValue":"0x","structLogs":[]}`},
		{name: "prestateTracer", receipt: receipt, config: tracerConfig("prestateTracer", ""), want: `{}`},
		{name: "prestateTracer diffMode", receipt: receipt, config: tracerConfig("prestateTracer", `{"diffMode":true}`), want: `{"pre":{},"post":{}}`},
		{name: "invalid tracer config", receipt: receipt, config: tracerConfig("prestateTracer", `{"diffMode":`), want: `{}`},
		{name: "4byteTracer", receipt: receipt, config: tracerConfig("4byteTracer", ""), want: `null`},
		{name: "js tracer", receipt: receipt, config: tracerConfig("{result: function() { return 1 }}", ""), want: `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := syntheticTraceResult(tx, tc.receipt, tc.config, tc.errMsg)
			require.NoError(t, err)
			requireJSON(t, tc.want, got)
		})
	}

	t.Run("create", func(t *testing.T) {
		from := common.HexToAddress("0x1084")
		create := &rpctypes.RPCTransaction{From: from, Nonce: 7, Gas: 90000, Input: hexutil.Bytes{0x60, 0x80}, Value: (*hexutil.Big)(big.NewInt(5))}
		got, err := syntheticTraceResult(create, map[string]interface{}{"status": hexutil.Uint(0), "gasUsed": hexutil.Uint64(90000)}, callTracerConfig, "reverted: message 1 of the cosmos transaction failed")
		require.NoError(t, err)
		frame := frameMap(t, got)
		require.Equal(t, "CREATE", frame["type"])
		require.Equal(t, crypto.CreateAddress(from, 7).Hex(), common.HexToAddress(frame["to"].(string)).Hex())
		require.Equal(t, "0x5", frame["value"])
		require.Equal(t, "0x6080", frame["input"])
		require.Equal(t, "reverted: message 1 of the cosmos transaction failed", frame["error"])

		contract := common.HexToAddress("0xc0de")
		got, err = syntheticTraceResult(create, map[string]interface{}{"status": hexutil.Uint(1), "contractAddress": contract}, callTracerConfig, "")
		require.NoError(t, err)
		require.Equal(t, contract.Hex(), common.HexToAddress(frameMap(t, got)["to"].(string)).Hex())
	})

	t.Run("nil input and value", func(t *testing.T) {
		got, err := syntheticTraceResult(&rpctypes.RPCTransaction{To: tx.To}, receipt, callTracerConfig, "")
		require.NoError(t, err)
		frame := frameMap(t, got)
		require.Equal(t, "0x", frame["input"])
		require.Equal(t, "0x0", frame["value"])
	})

	t.Run("unencodable receipt", func(t *testing.T) {
		_, err := syntheticTraceResult(tx, map[string]interface{}{"status": make(chan int)}, callTracerConfig, "")
		require.Error(t, err)
		_, err = syntheticTraceResult(tx, map[string]interface{}{"status": "0xzz"}, callTracerConfig, "")
		require.Error(t, err)
	})
}

func TestIsLegacyTracePlaceholder(t *testing.T) {
	for _, tc := range []struct {
		entry *rpctypes.TxTraceResult
		want  bool
	}{
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": float64(0)}}, true},
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": 0}}, true},
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": int64(0)}}, true},
		{nil, false},
		{&rpctypes.TxTraceResult{Error: "x", Result: map[string]interface{}{"type": 0}}, false},
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": "CALL"}}, false},
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": float64(1)}}, false},
		{&rpctypes.TxTraceResult{Result: map[string]interface{}{"type": 0, "from": "0x"}}, false},
		{&rpctypes.TxTraceResult{Result: []interface{}{}}, false},
	} {
		require.Equal(t, tc.want, isLegacyTracePlaceholder(tc.entry), "%#v", tc.entry)
	}
}

// TestAlignTraceResults covers the alignment with a visible tx view.
func TestAlignTraceResults(t *testing.T) {
	virtualTx, ethTx, missingEth, legacyVirtual, broken, anteFailed :=
		common.HexToHash("0x01"), common.HexToHash("0x02"), common.HexToHash("0x03"),
		common.HexToHash("0x04"), common.HexToHash("0x05"), common.HexToHash("0x06")

	vTx, vReceipt := syntheticFixtureTx()
	eTo := common.HexToAddress("0xe0")
	eTx := &rpctypes.RPCTransaction{From: common.HexToAddress("0xf0"), To: &eTo, Gas: 21000, Input: hexutil.Bytes{}}
	eReceipt := map[string]interface{}{"status": hexutil.Uint(1), "gasUsed": hexutil.Uint64(21000)}
	view := &traceTxView{
		hashes: []common.Hash{virtualTx, ethTx, missingEth, legacyVirtual, broken},
		lookup: func(hash common.Hash) (*rpctypes.RPCTransaction, map[string]interface{}, error) {
			switch hash {
			case virtualTx, legacyVirtual:
				return vTx, vReceipt, nil
			case missingEth:
				return eTx, eReceipt, nil
			case broken:
				return vTx, map[string]interface{}{"status": "0xzz"}, nil
			}
			return nil, nil, errFixtureUnavailable
		},
	}

	ethResult := &rpctypes.TxTraceResult{TxHash: ethTx, Result: map[string]interface{}{"type": "CALL", "tag": "first"}}
	duplicate := &rpctypes.TxTraceResult{TxHash: ethTx, Result: map[string]interface{}{"type": "CALL", "tag": "dup"}}
	hidden := &rpctypes.TxTraceResult{TxHash: anteFailed, Error: "insufficient balance for transfer"}
	legacy := &rpctypes.TxTraceResult{TxHash: legacyVirtual, Result: map[string]interface{}{"type": float64(0)}}

	b := newFixtureBackend(t, fixtureBackendOptions{offline: true})
	aligned := b.alignTraceResults([]*rpctypes.TxTraceResult{hidden, ethResult, duplicate, legacy}, view, callTracerConfig)
	require.Equal(t, view.hashes, traceHashes(aligned))
	require.Equal(t, "CALL", frameMap(t, aligned[0].Result)["type"], "virtual tx gets a frame")
	require.Same(t, ethResult, aligned[1], "duplicate hashes keep the first entry")
	missing := frameMap(t, aligned[2].Result)
	require.Equal(t, traceUnavailableError, missing["error"], "missing ethereum trace gets a failed frame")
	require.Equal(t, "0x00000000000000000000000000000000000000e0", missing["to"])
	require.Equal(t, frameMap(t, aligned[0].Result), frameMap(t, aligned[3].Result), "legacy placeholder is rebuilt")
	require.Equal(t, traceUnavailableError, aligned[4].Error, "unbuildable trace gets an entry-level error")
	require.Nil(t, aligned[4].Result)

	unknown := &traceTxView{hashes: []common.Hash{anteFailed}, lookup: view.lookup}
	got := b.alignTraceResults(nil, unknown, callTracerConfig)
	require.Len(t, got, 1)
	require.Equal(t, traceUnavailableError, got[0].Error, "tx lookup failure")

	passthrough := []*rpctypes.TxTraceResult{hidden}
	require.Equal(t, passthrough, b.alignTraceResults(passthrough, nil, callTracerConfig), "no view")
	require.Empty(t, b.alignTraceResults(passthrough, &traceTxView{}, callTracerConfig), "block without visible txs")
	require.NotNil(t, b.alignTraceResults(passthrough, &traceTxView{}, callTracerConfig))
	withNil := []*rpctypes.TxTraceResult{nil, ethResult}
	require.Equal(t, withNil, b.alignTraceResults(withNil, view, callTracerConfig), "nil result")
	noHash := []*rpctypes.TxTraceResult{{Result: map[string]interface{}{"type": "CALL"}}}
	require.Equal(t, noHash, b.alignTraceResults(noHash, view, callTracerConfig), "result without hash")
}

// TestTraceBlockViewSources checks that the visible tx list of a block trace
// is the one eth_getBlockByNumber returns, for an indexed block and for a live
// one, in both virtualization modes.
func TestTraceBlockViewSources(t *testing.T) {
	for _, height := range mainnetfx.AllHeights {
		for _, virtualMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/virtual=%v", height, virtualMode), func(t *testing.T) {
				comet := newFixtureComet(t)
				resBlock, blockRes := comet.blocks[height], comet.results[height]

				live := newFixtureBackend(t, fixtureBackendOptions{virtual: virtualMode, comet: comet, query: liveQuery()})
				block, err := live.GetBlockByNumber(rpctypes.BlockNumber(height), false)
				require.NoError(t, err)
				want := blockHashes(t, block)
				view := live.traceBlockView(height, resBlock, blockRes)
				require.NotNil(t, view)
				require.Equal(t, want, view.hashes, "live view")
				comet.calls = map[string]int{}
				fetched := live.traceBlockView(height, nil, nil)
				require.NotNil(t, fetched)
				require.Equal(t, want, fetched.hashes, "live view fetches the block data")
				require.Equal(t, map[string]int{"Block": 1, "BlockResults": 1}, comet.calls)
				offline := newFixtureBackend(t, fixtureBackendOptions{virtual: virtualMode, offline: true, comet: comet})
				require.Nil(t, offline.traceBlockView(height, resBlock, blockRes), "offline without index")

				kv, _ := newFixtureKV(t, virtualMode)
				indexFixtureBlocks(t, kv, height)
				cached := newFixtureBackend(t, fixtureBackendOptions{virtual: virtualMode, indexer: kv, comet: newFixtureComet(t)})
				view = cached.traceBlockView(height, nil, nil)
				require.NotNil(t, view)
				require.Equal(t, want, view.hashes, "cached view")

				// a block indexed in the other mode is served live
				other := newFixtureBackend(t, fixtureBackendOptions{virtual: !virtualMode, indexer: kv, comet: comet, query: liveQuery()})
				require.Nil(t, other.cachedTraceBlockView(height))
				otherBlock, err := other.GetBlockByNumber(rpctypes.BlockNumber(height), false)
				require.NoError(t, err)
				require.Equal(t, blockHashes(t, otherBlock), other.traceBlockView(height, nil, nil).hashes)
			})
		}
	}

	// visible list lookup failure
	idx := &overrideIndexer{visibleErr: errFixtureUnavailable, visible: []common.Hash{mainnetfx.TxEx1}}
	b := newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: idx})
	require.Nil(t, b.traceBlockView(1, nil, nil))

	// a cached tx list inconsistent with the block meta (written by an older
	// indexer) is not served, and the view follows the live block like
	// eth_getBlockByNumber does
	height := mainnetfx.HeightEx1Failed
	kv, _ := newFixtureKV(t, true)
	indexFixtureBlocks(t, kv, height)
	visible, err := kv.GetRPCTransactionHashesByBlockHeight(height)
	require.NoError(t, err)
	stale := &overrideIndexer{TxIndexer: kv, visible: append(append([]common.Hash{}, visible...), mainnetfx.TxEx1)}
	comet := newFixtureComet(t)
	b = newFixtureBackend(t, fixtureBackendOptions{virtual: true, indexer: stale, comet: comet, query: liveQuery()})
	require.Nil(t, b.cachedTraceBlockView(height))
	block, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), false)
	require.NoError(t, err)
	require.Equal(t, visible, blockHashes(t, block))
	require.Equal(t, visible, b.traceBlockView(height, nil, nil).hashes)
	offline := newFixtureBackend(t, fixtureBackendOptions{virtual: true, offline: true, indexer: stale})
	require.Nil(t, offline.traceBlockView(height, nil, nil))
}

// TestTraceBlockCachedTraceOfUnindexedBlock serves a block trace cached while
// the block was not indexed (an EVM-only replay): online, the entries follow
// the live tx list, virtual txs included.
func TestTraceBlockCachedTraceOfUnindexedBlock(t *testing.T) {
	height := mainnetfx.HeightEx1Included
	kv, _ := newFixtureKV(t, true)
	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{virtual: true, indexer: kv, comet: comet, query: liveQuery()})
	replayed := replayedFixtureHashes(t, b, comet, height)
	entries := make([]map[string]interface{}, 0, len(replayed))
	for i, hash := range replayed {
		entries = append(entries, map[string]interface{}{"txHash": hash, "result": map[string]interface{}{"type": "CALL", "tag": fmt.Sprint(i)}})
	}
	raw, err := json.Marshal(entries)
	require.NoError(t, err)
	require.NoError(t, kv.SetTraceBlockByHeight(height, callTracerConfig, raw))

	got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	block, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), false)
	require.NoError(t, err)
	require.Equal(t, blockHashes(t, block), traceHashes(got))
	require.Greater(t, len(got), len(replayed))
	for _, entry := range got {
		require.Empty(t, entry.Error)
		require.IsType(t, "", frameMap(t, entry.Result)["type"])
	}
}

func blockHashes(t *testing.T, block map[string]interface{}) []common.Hash {
	t.Helper()
	require.NotNil(t, block)
	txs, ok := block["transactions"].([]interface{})
	require.True(t, ok, "transactions %T", block["transactions"])
	hashes := make([]common.Hash, 0, len(txs))
	for _, tx := range txs {
		hash, ok := tx.(common.Hash)
		require.True(t, ok, "tx %T", tx)
		hashes = append(hashes, hash)
	}
	return hashes
}

// replayedFixtureHashes returns the messages a block trace of height replays.
func replayedFixtureHashes(t *testing.T, b *Backend, comet *fixtureComet, height int64) []common.Hash {
	t.Helper()
	block := comet.blocks[height].Block
	return msgHashes(replayedMsgs(b.traceMsgs(block, comet.results[height].TxResults, len(block.Txs))))
}

// expectFixtureTraceBlock mocks the block trace of height with tagged frames,
// returning the replayed hashes.
func expectFixtureTraceBlock(t *testing.T, query *rpcmocks.EVMQueryClient, b *Backend, comet *fixtureComet, height int64) []common.Hash {
	t.Helper()
	replayed := replayedFixtureHashes(t, b, comet, height)
	if len(replayed) > 0 {
		query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, replayed)).
			Return(traceBlockResponse(t, len(replayed)), nil).Once()
	}
	// failed messages after other messages of their Cosmos tx are traced alone
	query.On("TraceTx", mock.Anything, mock.Anything).Return(traceTxResponse("alone"), nil).Maybe()
	return replayed
}

// TestTraceBlockVirtualFrames traces every fixture block with virtualization:
// one entry per visible tx in eth_getBlockByNumber order, every virtual tx with
// a geth CALL frame built from the tx and receipt served for it, the same as
// debug_traceTransaction returns, from the chain and from the cache, online
// and offline.
func TestTraceBlockVirtualFrames(t *testing.T) {
	for _, height := range mainnetfx.AllHeights {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			kv, _ := newFixtureKV(t, true)
			indexFixtureBlocks(t, kv, height)
			comet := newFixtureComet(t)
			query := &rpcmocks.EVMQueryClient{}
			b := newFixtureBackend(t, fixtureBackendOptions{virtual: true, indexer: kv, comet: comet, query: query})
			replayed := expectFixtureTraceBlock(t, query, b, comet, height)

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			require.NoError(t, err)
			query.AssertExpectations(t)
			block, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), false)
			require.NoError(t, err)
			require.Equal(t, blockHashes(t, block), traceHashes(got))

			isReplayed := make(map[common.Hash]bool)
			for _, hash := range replayed {
				isReplayed[hash] = true
			}
			virtualCount := 0
			for _, entry := range got {
				require.Empty(t, entry.Error, "entry-level error for %s", entry.TxHash.Hex())
				frame := frameMap(t, entry.Result)
				typ, ok := frame["type"].(string)
				require.True(t, ok, "frame type of %s must be a string: %v", entry.TxHash.Hex(), frame)
				require.NotEmpty(t, typ)
				if isReplayed[entry.TxHash] {
					continue
				}
				tx, err := b.GetTransactionByHash(entry.TxHash)
				require.NoError(t, err)
				if !tx.Virtual {
					continue
				}
				virtualCount++
				receipt, err := b.GetTransactionReceipt(entry.TxHash)
				require.NoError(t, err)
				want, err := syntheticTraceResult(tx, receipt, callTracerConfig, "")
				require.NoError(t, err)
				require.Equal(t, frameMap(t, want), frame)
				require.Equal(t, "CALL", typ)
				require.Equal(t, tx.From.Hex(), common.HexToAddress(frame["from"].(string)).Hex())
				require.Equal(t, tx.To.Hex(), common.HexToAddress(frame["to"].(string)).Hex())
				require.Equal(t, receipt["gasUsed"].(hexutil.Uint64).String(), frame["gasUsed"])
				if receipt["status"].(hexutil.Uint) == 0 {
					require.NotEmpty(t, frame["error"])
				} else {
					require.Nil(t, frame["error"])
				}

				trace, err := b.TraceTransaction(entry.TxHash, callTracerConfig)
				require.NoError(t, err)
				require.Equal(t, frame, frameMap(t, trace), "debug_traceTransaction(%s)", entry.TxHash.Hex())
			}
			require.Positive(t, virtualCount, "fixture blocks have virtual txs")

			// from the cache: no Comet or gRPC request (mock calls would panic)
			for _, offline := range []bool{false, true} {
				cometCached := newFixtureComet(t)
				cached := newFixtureBackend(t, fixtureBackendOptions{virtual: true, offline: offline, indexer: kv, comet: cometCached})
				again, err := cached.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
				require.NoError(t, err)
				require.Equal(t, frameList(t, got), frameList(t, again), "offline=%v", offline)
				for _, entry := range again {
					trace, err := cached.TraceTransaction(entry.TxHash, callTracerConfig)
					require.NoError(t, err)
					require.Equal(t, frameMap(t, entry.Result), frameMap(t, trace))
				}
				require.Empty(t, cometCached.calls, "offline=%v", offline)
			}

			// virtual txs are hidden when virtualization is off
			plain := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: newFixtureComet(t), offline: true})
			for _, entry := range got {
				if tx, _ := b.GetTransactionByHash(entry.TxHash); tx.Virtual {
					_, err := plain.TraceTransaction(entry.TxHash, callTracerConfig)
					require.Error(t, err)
					break
				}
			}
		})
	}
}

func frameList(t *testing.T, entries []*rpctypes.TxTraceResult) []map[string]interface{} {
	t.Helper()
	out := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		out = append(out, frameMap(t, entry))
	}
	return out
}

// TestTraceBlockVirtualFramesPerTracer checks the virtual entries of a block
// trace for the other tracers.
func TestTraceBlockVirtualFramesPerTracer(t *testing.T) {
	height := mainnetfx.HeightEx1Included
	for _, tc := range []struct {
		config *rpctypes.TraceConfig
		want   string
	}{
		{nil, `{"failed":false,"returnValue":"0x","structLogs":[]}`},
		{tracerConfig("prestateTracer", ""), `{}`},
		{tracerConfig("prestateTracer", `{"diffMode":true}`), `{"pre":{},"post":{}}`},
		{tracerConfig("4byteTracer", ""), `null`},
	} {
		name := "structLogger"
		if tc.config != nil {
			name = tc.config.Tracer + string(tc.config.TracerConfig)
		}
		t.Run(name, func(t *testing.T) {
			kv, _ := newFixtureKV(t, true)
			indexFixtureBlocks(t, kv, height)
			comet := newFixtureComet(t)
			query := &rpcmocks.EVMQueryClient{}
			b := newFixtureBackend(t, fixtureBackendOptions{virtual: true, indexer: kv, comet: comet, query: query})
			replayed := expectFixtureTraceBlock(t, query, b, comet, height)

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), tc.config, nil)
			require.NoError(t, err)
			require.Len(t, got, len(replayed)+countVirtual(t, b, got))
			for _, entry := range got {
				if tx, _ := b.GetTransactionByHash(entry.TxHash); !tx.Virtual {
					continue
				}
				require.Empty(t, entry.Error)
				// the entry keeps its result key, also for null results
				entryJSON, err := json.Marshal(entry)
				require.NoError(t, err)
				require.Contains(t, frameMap(t, json.RawMessage(entryJSON)), "result")
				bz, err := json.Marshal(entry.Result)
				require.NoError(t, err)
				if tc.config == nil {
					// the gas used is the receipt's
					var logger map[string]interface{}
					require.NoError(t, json.Unmarshal(bz, &logger))
					receipt, _ := b.GetTransactionReceipt(entry.TxHash)
					require.EqualValues(t, uint64(receipt["gasUsed"].(hexutil.Uint64)), logger["gas"])
					delete(logger, "gas")
					requireJSON(t, tc.want, logger)
					continue
				}
				require.JSONEq(t, tc.want, string(bz))
			}
		})
	}
}

func countVirtual(t *testing.T, b *Backend, entries []*rpctypes.TxTraceResult) int {
	t.Helper()
	n := 0
	for _, entry := range entries {
		tx, err := b.GetTransactionByHash(entry.TxHash)
		require.NoError(t, err)
		if tx.Virtual {
			n++
		}
	}
	return n
}

// TestTraceBlockLegacyPlaceholderCache serves a block trace cached by
// v1.4.3/v1.4.4, aligned with `{"type": 0}` placeholders for virtual txs: the
// placeholders are rebuilt as frames from the cache alone, and
// debug_traceTransaction finds each tx by hash.
func TestTraceBlockLegacyPlaceholderCache(t *testing.T) {
	height := mainnetfx.HeightEx2Included
	kv, _ := newFixtureKV(t, true)
	indexFixtureBlocks(t, kv, height)
	visible, err := kv.GetRPCTransactionHashesByBlockHeight(height)
	require.NoError(t, err)

	legacy := make([]map[string]interface{}, 0, len(visible))
	evm := 0
	for _, hash := range visible {
		virtualTx, err := kv.IsVirtualRPCTransaction(hash)
		require.NoError(t, err)
		result := map[string]interface{}{"type": 0}
		if !virtualTx {
			result = map[string]interface{}{"type": "CALL", "tag": fmt.Sprint(evm)}
			evm++
		}
		legacy = append(legacy, map[string]interface{}{"txHash": hash, "result": result})
	}
	require.Positive(t, evm)
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, kv.SetTraceBlockByHeight(height, callTracerConfig, raw))

	comet := newFixtureComet(t)
	b := newFixtureBackend(t, fixtureBackendOptions{virtual: true, offline: true, indexer: kv, comet: comet})
	got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	require.Equal(t, visible, traceHashes(got))
	evm = 0
	for _, entry := range got {
		frame := frameMap(t, entry.Result)
		require.IsType(t, "", frame["type"], "legacy placeholder left for %s", entry.TxHash.Hex())
		if tag, ok := frame["tag"]; ok {
			require.Equal(t, fmt.Sprint(evm), tag)
			evm++
		}
		trace, err := b.TraceTransaction(entry.TxHash, callTracerConfig)
		require.NoError(t, err)
		require.Equal(t, frame, frameMap(t, trace))
	}
	require.Empty(t, comet.calls)
}

// TestTraceBlockLiveVirtualUnindexed traces a block that is not indexed yet
// with virtualization: the entries follow the live eth_getBlockByNumber list,
// virtual txs included.
func TestTraceBlockLiveVirtualUnindexed(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	for _, withIndexer := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexer=%v", withIndexer), func(t *testing.T) {
			comet := newFixtureComet(t)
			query := liveQuery()
			opts := fixtureBackendOptions{virtual: true, comet: comet, query: query}
			if withIndexer {
				opts.indexer, _ = newFixtureKV(t, true)
			}
			b := newFixtureBackend(t, opts)
			expectFixtureTraceBlock(t, query, b, comet, height)

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			require.NoError(t, err)
			query.AssertExpectations(t)
			// the block results fetched for the replay also build the view
			require.Equal(t, map[string]int{"Block": 1, "BlockResults": 1}, comet.calls)
			block, err := b.GetBlockByNumber(rpctypes.BlockNumber(height), false)
			require.NoError(t, err)
			require.Equal(t, blockHashes(t, block), traceHashes(got))
			for _, entry := range got {
				require.Empty(t, entry.Error)
				require.IsType(t, "", frameMap(t, entry.Result)["type"])
			}
		})
	}
}
