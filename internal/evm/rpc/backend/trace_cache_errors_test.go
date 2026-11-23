package backend

import (
	"encoding/json"
	"fmt"
	"testing"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

func TestPersistentTraceError(t *testing.T) {
	for msg, want := range map[string]bool{
		status.Error(codes.Internal, "insufficient balance for transfer").Error():                             true,
		status.Error(codes.Internal, "execution reverted").Error():                                            true,
		status.Error(codes.Internal, "execution timeout").Error():                                             false,
		status.Error(codes.Internal, "failed: context deadline exceeded").Error():                             false,
		status.Error(codes.Internal, "context canceled").Error():                                              false,
		status.Error(codes.InvalidArgument, "timeout exceeding max value: 5s").Error():                        false,
		status.Error(codes.Unavailable, "connection error: desc = \"transport: connection refused\"").Error(): false,
		status.Error(codes.DeadlineExceeded, "context deadline exceeded").Error():                             false,
		status.Error(codes.Canceled, "context canceled").Error():                                              false,
		status.Error(codes.Unknown, "unknown service").Error():                                                false,
		traceUnavailableError: false,
		"EOF":                 false,
		"":                    false,
	} {
		require.Equal(t, want, persistentTraceError(msg), msg)
	}
	require.True(t, persistentTraceResults(nil))
	require.True(t, persistentTraceResults([]*rpctypes.TxTraceResult{nil, {Result: map[string]interface{}{}}, {Error: status.Error(codes.Internal, "x").Error()}}))
	require.False(t, persistentTraceResults([]*rpctypes.TxTraceResult{{Result: map[string]interface{}{}}, {Error: "EOF"}}))
}

// traceBlockResponseWithError is a tagged chain block trace whose first entry
// is the entry-level error errMsg.
func traceBlockResponseWithError(t *testing.T, n int, errMsg string) *evmtypes.QueryTraceBlockResponse {
	t.Helper()
	results := make([]map[string]interface{}, n)
	for i := range results {
		results[i] = map[string]interface{}{"result": map[string]interface{}{"type": "CALL", "tag": fmt.Sprint(i)}}
	}
	results[0] = map[string]interface{}{"error": errMsg}
	bz, err := json.Marshal(results)
	require.NoError(t, err)
	return &evmtypes.QueryTraceBlockResponse{Data: bz}
}

// TestTraceBlockCachesOnlyPersistentErrors: a block trace is cached only when
// its entry-level errors are tx outcomes reported by the chain; a transient one
// (the chain's trace deadline here) is returned but traced again next time.
func TestTraceBlockCachesOnlyPersistentErrors(t *testing.T) {
	height := mainnetfx.HeightEx1Included
	for _, tc := range []struct {
		errMsg string
		cached bool
	}{
		{status.Error(codes.Internal, "execution timeout").Error(), false},
		{status.Error(codes.Unavailable, "transport is closing").Error(), false},
		{status.Error(codes.Internal, "insufficient balance for transfer").Error(), true},
	} {
		t.Run(tc.errMsg, func(t *testing.T) {
			kv, _ := newFixtureKV(t, false)
			indexFixtureBlocks(t, kv, height)
			comet := newFixtureComet(t)
			query := &rpcmocks.EVMQueryClient{}
			b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
			replayed := replayedFixtureHashes(t, b, comet, height)
			query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, replayed)).
				Return(traceBlockResponseWithError(t, len(replayed), tc.errMsg), nil).Once()

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			require.NoError(t, err)
			require.Equal(t, tc.errMsg, got[0].Error)
			_, cacheErr := kv.GetTraceBlockByHeight(height, callTracerConfig)
			require.Equal(t, tc.cached, cacheErr == nil, "cached: %v", cacheErr)

			if !tc.cached {
				// the next request asks the chain again and caches its answer
				query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, replayed)).
					Return(traceBlockResponse(t, len(replayed)), nil).Once()
				again, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
				require.NoError(t, err)
				require.Empty(t, again[0].Error)
				_, cacheErr = kv.GetTraceBlockByHeight(height, callTracerConfig)
				require.NoError(t, cacheErr)
			}
			query.AssertExpectations(t)
		})
	}
}

// TestTraceBlockAloneTransientFailureNotCached: an error of the TraceTx call
// made for a message traced alone is never cached with the block (PR #26
// review), whatever its gRPC code: the transport and the server's panic
// recovery report Internal too. The block recovers on the next request.
func TestTraceBlockAloneTransientFailureNotCached(t *testing.T) {
	height := mainnetfx.HeightMultiMsgFailure
	before, after := multiMsgFixtureHashes(t)
	first, second := mainnetfx.TxMultiMsgReverted, mainnetfx.TxMultiMsgUnexecuted
	log := `failed to execute message; message index: 1: {"tx_hash":"` + second.Hex() + `","gas_used":21000,"vm_error":"execution reverted"}`

	for _, callErr := range []error{
		status.Error(codes.Unavailable, "connection error"),
		status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: INTERNAL_ERROR"),
	} {
		t.Run(callErr.Error(), func(t *testing.T) {
			kv, _ := newFixtureKV(t, false)
			indexFixtureBlocks(t, kv, height)
			comet := newFixtureComet(t)
			withMultiMsgLog(t, comet, log)
			query := &rpcmocks.EVMQueryClient{}
			query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, []common.Hash{before, after})).
				Return(traceBlockResponse(t, 2), nil).Twice()
			query.On("TraceTx", mock.Anything, traceTxMatcher(height, second, []common.Hash{before, first})).
				Return(nil, callErr).Once()
			b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})

			got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			require.NoError(t, err)
			require.Equal(t, callErr.Error(), got[2].Error)
			_, cacheErr := kv.GetTraceBlockByHeight(height, callTracerConfig)
			require.Error(t, cacheErr, "a block trace with a failed TraceTx call must not be cached")

			query.On("TraceTx", mock.Anything, traceTxMatcher(height, second, []common.Hash{before, first})).
				Return(traceTxResponse("second"), nil).Once()
			got, err = b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
			require.NoError(t, err)
			require.Empty(t, got[2].Error)
			require.Equal(t, "second", traceTag(t, got[2].Result))
			_, cacheErr = kv.GetTraceBlockByHeight(height, callTracerConfig)
			require.NoError(t, cacheErr)
			query.AssertExpectations(t)
		})
	}
}

// TestTraceWithoutBlockResultsNotCached: when the block results cannot be
// fetched, every message is replayed (ante-failed txs included), so the block
// and tx traces are returned as a best effort but not cached; the next request
// with block results replays the right messages and caches the trace.
func TestTraceWithoutBlockResultsNotCached(t *testing.T) {
	height := mainnetfx.HeightEx1Failed
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	comet := newFixtureComet(t)
	query := &rpcmocks.EVMQueryClient{}
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
	all := msgHashes(fixtureEthMsgs(t, height))
	filtered := replayedFixtureHashes(t, b, comet, height)
	require.Greater(t, len(all), len(filtered), "the fixture block has ante-failed txs")

	comet.resultsErr = errFixtureUnavailable
	query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, all)).
		Return(traceBlockResponse(t, len(all)), nil).Once()
	_, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	_, cacheErr := kv.GetTraceBlockByHeight(height, callTracerConfig)
	require.Error(t, cacheErr, "a replay without block results must not be cached")

	target := filtered[len(filtered)-1]
	query.On("TraceTx", mock.Anything, mock.MatchedBy(func(req *evmtypes.QueryTraceTxRequest) bool { return req.Msg.Hash() == target })).
		Return(traceTxResponse("best effort"), nil).Once()
	_, err = b.TraceTransaction(target, callTracerConfig)
	require.NoError(t, err)
	_, cacheErr = kv.GetTraceTransaction(target, callTracerConfig)
	require.Error(t, cacheErr, "a tx replay without block results must not be cached")

	comet.resultsErr = nil
	query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, filtered)).
		Return(traceBlockResponse(t, len(filtered)), nil).Once()
	_, err = b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	_, cacheErr = kv.GetTraceBlockByHeight(height, callTracerConfig)
	require.NoError(t, cacheErr)
	query.AssertExpectations(t)
}

// TestTraceBlockRetracesCachedTransientErrors: a block trace cached with a
// transient error (by an earlier version) is traced again online and replaced;
// offline, it is served as cached.
func TestTraceBlockRetracesCachedTransientErrors(t *testing.T) {
	height := mainnetfx.HeightEx1Included
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	comet := newFixtureComet(t)
	query := &rpcmocks.EVMQueryClient{}
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
	replayed := replayedFixtureHashes(t, b, comet, height)

	stale := make([]map[string]interface{}, len(replayed))
	for i, hash := range replayed {
		stale[i] = map[string]interface{}{"txHash": hash, "result": map[string]interface{}{"type": "CALL", "tag": "stale"}}
	}
	timeout := status.Error(codes.Internal, "execution timeout").Error()
	stale[0] = map[string]interface{}{"txHash": replayed[0], "error": timeout}
	raw, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, kv.SetTraceBlockByHeight(height, callTracerConfig, raw))

	offline := newFixtureBackend(t, fixtureBackendOptions{offline: true, indexer: kv})
	got, err := offline.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	require.Equal(t, timeout, got[0].Error, "offline serves the cache as is")

	query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, replayed)).
		Return(traceBlockResponse(t, len(replayed)), nil).Once()
	got, err = b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	query.AssertExpectations(t)
	require.Empty(t, got[0].Error)
	require.Equal(t, "a", traceTag(t, got[0].Result))

	// replaced in the cache: served without the chain from now on
	got, err = offline.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	require.Empty(t, got[0].Error)
}

// TestTraceBlockServesCachedTraceWhenRetraceFails: a block trace cached with a
// transient error is still served when tracing it again fails.
func TestTraceBlockServesCachedTraceWhenRetraceFails(t *testing.T) {
	height := mainnetfx.HeightEx1Included
	kv, _ := newFixtureKV(t, false)
	indexFixtureBlocks(t, kv, height)
	comet := newFixtureComet(t)
	query := &rpcmocks.EVMQueryClient{}
	b := newFixtureBackend(t, fixtureBackendOptions{indexer: kv, comet: comet, query: query})
	replayed := replayedFixtureHashes(t, b, comet, height)

	stale := make([]map[string]interface{}, len(replayed))
	for i, hash := range replayed {
		stale[i] = map[string]interface{}{"txHash": hash, "result": map[string]interface{}{"type": "CALL", "tag": "stale"}}
	}
	timeout := status.Error(codes.Internal, "execution timeout").Error()
	stale[0] = map[string]interface{}{"txHash": replayed[0], "error": timeout}
	raw, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, kv.SetTraceBlockByHeight(height, callTracerConfig, raw))

	query.On("TraceBlock", mock.Anything, traceBlockMatcher(height, replayed)).
		Return(nil, status.Error(codes.Unavailable, "connection refused")).Once()
	got, err := b.TraceBlock(rpctypes.BlockNumber(height), callTracerConfig, nil)
	require.NoError(t, err)
	query.AssertExpectations(t)
	require.Equal(t, timeout, got[0].Error)
	if len(replayed) > 1 {
		require.Equal(t, "stale", traceTag(t, got[1].Result))
	}
}
