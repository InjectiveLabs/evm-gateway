package backend

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	cmtjson "github.com/cometbft/cometbft/libs/json"
	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
	chainclient "github.com/InjectiveLabs/sdk-go/client/chain"
)

// Mainnet block 185556799: Cosmos tx #131 carries Ethereum tx
// anteFailedTxHash, which failed in the ante handler (code 5, insufficient
// funds, no events). An external indexer got stuck on its
// debug_traceTransaction error.
const (
	anteFailedHeight  int64  = 185556799
	anteFailedTxIndex uint32 = 131
	// node error returned when the ante-failed tx is replayed
	anteFailedNodeError = "rpc error: code = Internal desc = insufficient balance for transfer"
)

var anteFailedTxHash = common.HexToHash("0xa3151a9b8c52a0c4f741f7cd908b30e8e1de5f54770c3092090457f56f927c03")

func readGzipFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	bz, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return bz
}

type anteFailedFixture struct {
	block   *cmrpctypes.ResultBlock
	results *cmrpctypes.ResultBlockResults
	// ethMsgs lists the block's Ethereum messages in block order with the
	// index of their Cosmos tx (the block trace order).
	ethMsgs []fixtureEthMsg
}

type fixtureEthMsg struct {
	txIndex uint32
	msg     *evmtypes.MsgEthereumTx
}

func loadAnteFailedFixture(t *testing.T) *anteFailedFixture {
	t.Helper()
	fx := &anteFailedFixture{block: &cmrpctypes.ResultBlock{}, results: &cmrpctypes.ResultBlockResults{}}
	if err := cmtjson.Unmarshal(readGzipFixture(t, fmt.Sprintf("block_%d.json.gz", anteFailedHeight)), fx.block); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if err := cmtjson.Unmarshal(readGzipFixture(t, fmt.Sprintf("block_results_%d.json.gz", anteFailedHeight)), fx.results); err != nil {
		t.Fatalf("decode block results: %v", err)
	}
	clientCtx := fixtureClientContext(t)
	for txIndex, txBz := range fx.block.Block.Txs {
		tx, err := clientCtx.TxConfig.TxDecoder()(txBz)
		if err != nil {
			t.Fatalf("decode tx %d: %v", txIndex, err)
		}
		for _, msg := range tx.GetMsgs() {
			if ethMsg, ok := msg.(*evmtypes.MsgEthereumTx); ok {
				fx.ethMsgs = append(fx.ethMsgs, fixtureEthMsg{txIndex: uint32(txIndex), msg: ethMsg})
			}
		}
	}
	return fx
}

// position returns the block trace position of a tx hash.
func (fx *anteFailedFixture) position(t *testing.T, hash common.Hash) int {
	t.Helper()
	for i, m := range fx.ethMsgs {
		if m.msg.Hash() == hash {
			return i
		}
	}
	t.Fatalf("tx %s not in fixture block", hash.Hex())
	return -1
}

// executedFailure returns an Ethereum tx of the block that failed after the
// ante handler (it has ante events), with its Cosmos tx index.
func (fx *anteFailedFixture) executedFailure(t *testing.T) fixtureEthMsg {
	t.Helper()
	for _, m := range fx.ethMsgs {
		res := fx.results.TxResults[m.txIndex]
		if res.Code != 0 && !rpctypes.TxAnteFailed(res) {
			return m
		}
	}
	t.Fatalf("no executed failure in fixture block")
	return fixtureEthMsg{}
}

func fixtureClientContext(t *testing.T) client.Context {
	t.Helper()
	clientCtx, err := chainclient.NewClientContext("", "", nil)
	if err != nil {
		t.Fatalf("NewClientContext: %v", err)
	}
	return clientCtx
}

// fixtureCometClient serves the fixture block and block results.
type fixtureCometClient struct {
	cmrpcclient.Client
	fx                *anteFailedFixture
	blockCalls        atomic.Int64
	blockResultsCalls atomic.Int64
}

func (c *fixtureCometClient) Block(_ context.Context, height *int64) (*cmrpctypes.ResultBlock, error) {
	c.blockCalls.Add(1)
	if height == nil || *height != anteFailedHeight {
		return nil, fmt.Errorf("height %v not available", height)
	}
	return c.fx.block, nil
}

func (c *fixtureCometClient) BlockResults(_ context.Context, height *int64) (*cmrpctypes.ResultBlockResults, error) {
	c.blockResultsCalls.Add(1)
	if height == nil || *height != anteFailedHeight {
		return nil, fmt.Errorf("height %v not available", height)
	}
	return c.fx.results, nil
}

func (c *fixtureCometClient) TxSearch(context.Context, string, bool, *int, *int, string) (*cmrpctypes.ResultTxSearch, error) {
	return &cmrpctypes.ResultTxSearch{}, nil
}

// fixtureTraceIndexer resolves tx hashes of the fixture block and serves an
// optional cached block trace.
type fixtureTraceIndexer struct {
	indexer.TxIndexer
	fx          *anteFailedFixture
	cachedBlock json.RawMessage
}

func (i *fixtureTraceIndexer) WithContext(context.Context) indexer.TxIndexer { return i }

func (i *fixtureTraceIndexer) GetByTxHash(hash common.Hash) (*chaintypes.TxResult, error) {
	for _, m := range i.fx.ethMsgs {
		if m.msg.Hash() == hash {
			return &chaintypes.TxResult{Height: anteFailedHeight, TxIndex: m.txIndex, MsgIndex: 0, EthTxIndex: -1}, nil
		}
	}
	return nil, errors.New("tx not found")
}

func (i *fixtureTraceIndexer) GetTraceTransaction(common.Hash, *rpctypes.TraceConfig) (json.RawMessage, error) {
	return nil, indexer.ErrCacheMiss
}

func (i *fixtureTraceIndexer) SetTraceTransaction(common.Hash, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}

func (i *fixtureTraceIndexer) GetTraceBlockByHeight(int64, *rpctypes.TraceConfig) (json.RawMessage, error) {
	if i.cachedBlock != nil {
		return i.cachedBlock, nil
	}
	return nil, indexer.ErrCacheMiss
}

func (i *fixtureTraceIndexer) SetTraceBlockByHeight(int64, *rpctypes.TraceConfig, json.RawMessage) error {
	return nil
}

type anteFailedTestEnv struct {
	backend *Backend
	comet   *fixtureCometClient
	query   *rpcmocks.EVMQueryClient
	idx     *fixtureTraceIndexer
	fx      *anteFailedFixture
}

func newAnteFailedTestEnv(t *testing.T) *anteFailedTestEnv {
	t.Helper()
	fx := loadAnteFailedFixture(t)
	comet := &fixtureCometClient{fx: fx}
	query := &rpcmocks.EVMQueryClient{}
	idx := &fixtureTraceIndexer{fx: fx}
	b := &Backend{
		logger:      backendTestLogger(),
		cfg:         appconfig.Config{},
		clientCtx:   fixtureClientContext(t).WithClient(comet),
		queryClient: &rpctypes.QueryClient{QueryClient: query},
		indexer:     idx,
		chainID:     big.NewInt(1776),
	}
	return &anteFailedTestEnv{backend: b, comet: comet, query: query, idx: idx, fx: fx}
}

func traceConfig(tracer string) *rpctypes.TraceConfig {
	return &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: tracer}}
}

func marshalMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	bz, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(bz, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestFixtureHasExactlyOneAnteFailedTx(t *testing.T) {
	fx := loadAnteFailedFixture(t)
	var anteFailed []uint32
	executedFailures := 0
	for i, res := range fx.results.TxResults {
		if rpctypes.TxAnteFailed(res) {
			anteFailed = append(anteFailed, uint32(i))
		} else if res.Code != 0 {
			executedFailures++
		}
	}
	if len(anteFailed) != 1 || anteFailed[0] != anteFailedTxIndex {
		t.Fatalf("unexpected ante-failed txs %v", anteFailed)
	}
	if executedFailures == 0 {
		t.Fatalf("fixture must contain failures after the ante handler")
	}
	res := fx.results.TxResults[anteFailedTxIndex]
	if res.Code != 5 || res.Codespace != "sdk" || len(res.Events) != 0 || !strings.Contains(res.Log, "insufficient funds") {
		t.Fatalf("unexpected ante-failed result: %+v", res)
	}
	if fx.ethMsgs[fx.position(t, anteFailedTxHash)].txIndex != anteFailedTxIndex {
		t.Fatalf("ante-failed tx is not cosmos tx %d", anteFailedTxIndex)
	}
}

func TestTraceTransactionAnteFailedCallTracer(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.query.On("TraceTx", mock.Anything, mock.Anything).Return(nil, errors.New(anteFailedNodeError))

	trace, err := env.backend.TraceTransaction(anteFailedTxHash, traceConfig("callTracer"))
	if err != nil {
		t.Fatalf("expected a trace for the ante-failed tx, got %v", err)
	}
	frame := marshalMap(t, trace)
	want := map[string]interface{}{
		"type":    "CALL",
		"from":    "0xa193925d4d6702d1595a0de635c913b06bf38fb0",
		"value":   "0xd57271121b0000",
		"gas":     "0xc350",
		"gasUsed": "0xc350",
	}
	for key, value := range want {
		if got, _ := frame[key].(string); !strings.EqualFold(got, value.(string)) {
			t.Fatalf("frame %s = %v want %v (frame %v)", key, frame[key], value, frame)
		}
	}
	if to, _ := frame["to"].(string); to == "" {
		t.Fatalf("frame must carry the recipient: %v", frame)
	}
	if _, ok := frame["input"].(string); !ok {
		t.Fatalf("frame must carry the input: %v", frame)
	}
	if msg, _ := frame["error"].(string); !strings.Contains(msg, "insufficient funds") {
		t.Fatalf("frame must carry the ante error: %v", frame)
	}
	env.query.AssertExpectations(t)
}

func TestTraceTransactionAnteFailedStructLogger(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.query.On("TraceTx", mock.Anything, mock.Anything).Return(nil, errors.New(anteFailedNodeError))

	trace, err := env.backend.TraceTransaction(anteFailedTxHash, nil)
	if err != nil {
		t.Fatalf("expected a trace for the ante-failed tx, got %v", err)
	}
	got := marshalMap(t, trace)
	if got["failed"] != true || got["gas"] != float64(50000) || got["returnValue"] != "" {
		t.Fatalf("unexpected struct log trace: %v", got)
	}
	if logs, ok := got["structLogs"].([]interface{}); !ok || len(logs) != 0 {
		t.Fatalf("expected empty struct logs: %v", got)
	}
}

func TestTraceTransactionAnteFailedOtherTracersKeepError(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.query.On("TraceTx", mock.Anything, mock.Anything).Return(nil, errors.New(anteFailedNodeError))

	if _, err := env.backend.TraceTransaction(anteFailedTxHash, traceConfig("prestateTracer")); err == nil || err.Error() != anteFailedNodeError {
		t.Fatalf("expected the node error for an unsupported tracer, got %v", err)
	}
}

func TestTraceTransactionKeepsErrorsOfExecutedTxs(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	executed := env.fx.executedFailure(t)
	nodeErr := errors.New("rpc error: code = Internal desc = something else")
	env.query.On("TraceTx", mock.Anything, mock.Anything).Return(nil, nodeErr)

	if _, err := env.backend.TraceTransaction(executed.msg.Hash(), traceConfig("callTracer")); err == nil || err.Error() != nodeErr.Error() {
		t.Fatalf("a trace error of an executed tx must be returned, got %v", err)
	}
}

func TestTraceTransactionSuccessSkipsAnteFailedLookup(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	executed := env.fx.executedFailure(t)
	env.query.On("TraceTx", mock.Anything, mock.Anything).Return(&evmtypes.QueryTraceTxResponse{Data: []byte(`{"type":"CALL","error":"execution reverted"}`)}, nil)

	trace, err := env.backend.TraceTransaction(executed.msg.Hash(), traceConfig("callTracer"))
	if err != nil {
		t.Fatalf("TraceTransaction: %v", err)
	}
	if got := marshalMap(t, trace); got["error"] != "execution reverted" {
		t.Fatalf("node trace must be returned unchanged: %v", got)
	}
	if calls := env.comet.blockResultsCalls.Load(); calls != 0 {
		t.Fatalf("successful traces must not look up block results, got %d calls", calls)
	}
}

func TestTraceTransactionAnteFailedOfflineKeepsError(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.backend.cfg.OfflineRPCOnly = true

	if _, err := env.backend.TraceTransaction(anteFailedTxHash, traceConfig("callTracer")); err == nil {
		t.Fatalf("offline mode can't resolve ante failures and must keep the error")
	}
	if trace, ok := env.backend.anteFailedTxTrace(anteFailedTxHash, traceConfig("callTracer")); ok {
		t.Fatalf("offline mode must not build a trace: %v", trace)
	}
}

func TestAnteFailedTxTraceUnresolvable(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	if _, ok := env.backend.anteFailedTxTrace(common.HexToHash("0x01"), traceConfig("callTracer")); ok {
		t.Fatalf("unknown tx must not get a trace")
	}

	env.comet.fx = &anteFailedFixture{
		block:   env.fx.block,
		results: &cmrpctypes.ResultBlockResults{Height: anteFailedHeight},
	}
	if _, ok := env.backend.anteFailedTxTrace(anteFailedTxHash, traceConfig("callTracer")); ok {
		t.Fatalf("missing tx result must not get a trace")
	}
}

// blockTraceResponse builds the node's block trace: CALL frames, and an error
// entry for the ante-failed tx.
func blockTraceResponse(t *testing.T, fx *anteFailedFixture) []byte {
	t.Helper()
	entries := make([]map[string]interface{}, len(fx.ethMsgs))
	for i, m := range fx.ethMsgs {
		if m.msg.Hash() == anteFailedTxHash {
			entries[i] = map[string]interface{}{"error": anteFailedNodeError}
			continue
		}
		entries[i] = map[string]interface{}{"result": map[string]interface{}{"type": "CALL", "tag": i}}
	}
	bz, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal block trace: %v", err)
	}
	return bz
}

func assertFilledBlockTrace(t *testing.T, fx *anteFailedFixture, results []*rpctypes.TxTraceResult) {
	t.Helper()
	if len(results) != len(fx.ethMsgs) {
		t.Fatalf("unexpected block trace length %d want %d", len(results), len(fx.ethMsgs))
	}
	failedAt := fx.position(t, anteFailedTxHash)
	for i, result := range results {
		if result.Error != "" {
			t.Fatalf("entry %d still has an error: %s", i, result.Error)
		}
		got := marshalMap(t, result.Result)
		if i == failedAt {
			if got["type"] != "CALL" || got["gasUsed"] != "0xc350" || !strings.Contains(fmt.Sprint(got["error"]), "insufficient funds") {
				t.Fatalf("ante-failed entry %d: %v", i, got)
			}
			continue
		}
		if got["tag"] != float64(i) {
			t.Fatalf("entry %d must be the node trace: %v", i, got)
		}
	}
}

func TestTraceBlockFillsAnteFailedEntries(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.query.On("TraceBlock", mock.Anything, mock.Anything).Return(&evmtypes.QueryTraceBlockResponse{Data: blockTraceResponse(t, env.fx)}, nil)

	results, err := env.backend.TraceBlock(rpctypes.BlockNumber(anteFailedHeight), traceConfig("callTracer"), env.fx.block)
	if err != nil {
		t.Fatalf("TraceBlock: %v", err)
	}
	assertFilledBlockTrace(t, env.fx, results)
}

func TestTraceBlockFillsCachedAnteFailedEntries(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.idx.cachedBlock = blockTraceResponse(t, env.fx)

	results, err := env.backend.TraceBlock(rpctypes.BlockNumber(anteFailedHeight), traceConfig("callTracer"), nil)
	if err != nil {
		t.Fatalf("TraceBlock: %v", err)
	}
	assertFilledBlockTrace(t, env.fx, results)
}

func TestTraceBlockLeavesEntriesWhenNotResolvable(t *testing.T) {
	env := newAnteFailedTestEnv(t)

	// no error entries: no lookups at all
	clean := []*rpctypes.TxTraceResult{{Result: map[string]interface{}{"type": "CALL"}}}
	env.backend.fillAnteFailedBlockTraces(clean, rpctypes.BlockNumber(anteFailedHeight), nil, traceConfig("callTracer"))
	if env.comet.blockCalls.Load() != 0 || env.comet.blockResultsCalls.Load() != 0 {
		t.Fatalf("block traces without errors must not trigger lookups")
	}

	// entry count doesn't match the block's Ethereum txs
	mismatched := []*rpctypes.TxTraceResult{{Error: anteFailedNodeError}}
	env.backend.fillAnteFailedBlockTraces(mismatched, rpctypes.BlockNumber(anteFailedHeight), env.fx.block, traceConfig("callTracer"))
	if mismatched[0].Error != anteFailedNodeError {
		t.Fatalf("mismatched block trace must be left as is")
	}

	// unsupported tracer keeps the error entry
	var entries []*rpctypes.TxTraceResult
	if err := json.Unmarshal(blockTraceResponse(t, env.fx), &entries); err != nil {
		t.Fatalf("decode entries: %v", err)
	}
	env.backend.fillAnteFailedBlockTraces(entries, rpctypes.BlockNumber(anteFailedHeight), env.fx.block, traceConfig("prestateTracer"))
	if entries[env.fx.position(t, anteFailedTxHash)].Error != anteFailedNodeError {
		t.Fatalf("unsupported tracer must keep the error entry")
	}

	// block not available
	errEntries := []*rpctypes.TxTraceResult{{Error: "boom"}}
	env.backend.fillAnteFailedBlockTraces(errEntries, rpctypes.BlockNumber(1), nil, traceConfig("callTracer"))
	if errEntries[0].Error != "boom" {
		t.Fatalf("unresolvable block must be left as is")
	}
}

func TestAnteFailedTraceResultShapes(t *testing.T) {
	fx := loadAnteFailedFixture(t)
	msg := fx.ethMsgs[fx.position(t, anteFailedTxHash)].msg
	res := fx.results.TxResults[anteFailedTxIndex]

	if _, ok := anteFailedTraceResult(nil, res, nil); ok {
		t.Fatalf("nil message must not produce a trace")
	}
	if _, ok := anteFailedTraceResult(msg, nil, nil); ok {
		t.Fatalf("nil result must not produce a trace")
	}

	// sender recovered from the signature when From is not set
	unsigned := *msg
	unsigned.From = nil
	trace, ok := anteFailedTraceResult(&unsigned, res, traceConfig("callTracer"))
	if !ok || !strings.EqualFold(marshalMap(t, trace)["from"].(string), "0xa193925d4d6702d1595a0de635c913b06bf38fb0") {
		t.Fatalf("sender must be recovered from the signature: %v", trace)
	}
}

func TestAnteFailedTraceResultContractCreation(t *testing.T) {
	fx := loadAnteFailedFixture(t)
	res := fx.results.TxResults[anteFailedTxIndex]
	create := evmtypes.NewTx(big.NewInt(1776), 7, nil, big.NewInt(0), 90000, big.NewInt(1), nil, nil, []byte{0x60, 0x80}, nil)
	create.From = common.HexToAddress("0x0000000000000000000000000000000000000abc").Bytes()

	trace, ok := anteFailedTraceResult(create, res, traceConfig("callTracer"))
	if !ok {
		t.Fatalf("expected a trace for a contract creation")
	}
	got := marshalMap(t, trace)
	if got["type"] != "CREATE" || got["gasUsed"] != "0x15f90" || got["input"] != "0x6080" {
		t.Fatalf("unexpected creation frame: %v", got)
	}
	if _, hasTo := got["to"]; hasTo {
		t.Fatalf("a creation frame without execution has no recipient: %v", got)
	}

	// unsigned message without From: the sender can't be recovered
	unsigned := evmtypes.NewTx(big.NewInt(1776), 7, nil, big.NewInt(0), 90000, big.NewInt(1), nil, nil, nil, nil)
	if _, ok := anteFailedTraceResult(unsigned, res, traceConfig("callTracer")); ok {
		t.Fatalf("an unrecoverable sender must not produce a call frame")
	}
}

func TestTraceBlockPropagatesNodeErrors(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	env.query.On("TraceBlock", mock.Anything, mock.Anything).Return(nil, errors.New("node down"))
	if _, err := env.backend.TraceBlock(rpctypes.BlockNumber(anteFailedHeight), traceConfig("callTracer"), env.fx.block); err == nil || err.Error() != "node down" {
		t.Fatalf("expected the node error, got %v", err)
	}
}

func TestAnteFailedTxTraceRequiresMatchingEthereumMessage(t *testing.T) {
	env := newAnteFailedTestEnv(t)
	// point the ante-failed hash at another Cosmos tx of the block
	env.idx.fx = &anteFailedFixture{ethMsgs: []fixtureEthMsg{{txIndex: anteFailedTxIndex - 1, msg: env.fx.ethMsgs[env.fx.position(t, anteFailedTxHash)].msg}}}
	if _, ok := env.backend.anteFailedTxTrace(anteFailedTxHash, traceConfig("callTracer")); ok {
		t.Fatalf("a tx index that doesn't hold the hash must not produce a trace")
	}
}
