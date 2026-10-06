package backend

import (
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"testing"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/indexer"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/mock"
)

func TestNormalizeTraceTimeoutPreservesCallerConfig(t *testing.T) {
	config := &rpctypes.TraceConfig{
		TraceConfig: evmtypes.TraceConfig{
			Tracer: "callTracer", Timeout: "10000s", Reexec: 128,
			DisableStack: true, DisableStorage: true, Debug: true, Limit: 42,
			Overrides: &evmtypes.ChainConfig{}, EnableMemory: true, EnableReturnData: true,
			TracerJsonConfig: `{"onlyTopCall":true}`,
			StateOverrides:   []byte(`{"embeddedState":true}`),
			BlockOverrides:   []byte(`{"embeddedBlock":true}`),
		},
		TracerConfig:   json.RawMessage(`{"withLog":true}`),
		StateOverrides: json.RawMessage(`{"0x01":{"balance":"0x1"}}`),
		BlockOverrides: json.RawMessage(`{"number":"0x2"}`),
	}
	before := *config
	want := before
	want.Timeout = "30s"

	got := normalizeTraceTimeout(config)
	if got == config {
		t.Fatal("normalization must copy an oversized config before changing its timeout")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("normalization changed fields other than timeout: got %#v, want %#v", *got, want)
	}
	if !reflect.DeepEqual(*config, before) {
		t.Fatalf("normalization mutated the caller's config: got %#v, want %#v", *config, before)
	}
}

func TestNormalizeTraceTimeoutBoundaries(t *testing.T) {
	if got := normalizeTraceTimeout(nil); got != nil {
		t.Fatalf("nil config changed to %#v", got)
	}
	for _, timeout := range []string{"", "1ms", "29s", "30s", "30000ms", "0s", "-1s", "invalid", "999999999999999999999999s"} {
		t.Run(timeout, func(t *testing.T) {
			config := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Timeout: timeout}}
			if got := normalizeTraceTimeout(config); got != config || got.Timeout != timeout {
				t.Fatalf("timeout %q should be passed through unchanged, got %#v", timeout, got)
			}
		})
	}
	for _, timeout := range []string{"30.000000001s", "30001ms", "10000s"} {
		t.Run(timeout, func(t *testing.T) {
			config := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Timeout: timeout}}
			got := normalizeTraceTimeout(config)
			if got.Timeout != "30s" || config.Timeout != timeout {
				t.Fatalf("timeout %q: normalized=%q original=%q", timeout, got.Timeout, config.Timeout)
			}
		})
	}
}

func TestTraceBlockNormalizesTimeoutBeforeCacheAndForwarding(t *testing.T) {
	const height int64 = 186090216
	config := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: "callTracer", Timeout: "10000s"}}
	normalized := *config
	normalized.Timeout = "30s"
	oldFailure := json.RawMessage(`[{"error":"rpc error: code = InvalidArgument desc = timeout exceeding max value: 30s"}]`)
	success := json.RawMessage(`[{"result":{"type":"CALL","gasUsed":"0x5208"}}]`)
	idx := newTimeoutTraceIndexer(t)
	if err := idx.TxIndexer.SetTraceBlockByHeight(height, config, oldFailure); err != nil {
		t.Fatalf("seed oversized failure: %v", err)
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, ethMsg := mustSignedTraceMsg(t, &ethtypes.DynamicFeeTx{
		ChainID: big.NewInt(1776), GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2),
		Gas: 21000, To: ptrAddress(common.HexToAddress("0x1")), Value: big.NewInt(0),
	}, ethtypes.LatestSignerForChainID(big.NewInt(1776)), key)
	query := &rpcmocks.EVMQueryClient{}
	query.On("TraceBlock", mock.Anything, mock.MatchedBy(func(req *evmtypes.QueryTraceBlockRequest) bool {
		return req.BlockNumber == height && len(req.Txs) == 1 && req.Txs[0] == ethMsg &&
			req.TraceConfig != nil && req.TraceConfig.Timeout == "30s" && req.TraceConfig.Tracer == "callTracer"
	})).Return(&evmtypes.QueryTraceBlockResponse{Data: success}, nil).Once()
	b := &Backend{
		logger: backendTestLogger(), cfg: appconfig.Config{}, indexer: idx,
		clientCtx: client.Context{}.WithTxConfig(backendTraceTestTxConfig{
			decoder: func([]byte) (sdk.Tx, error) {
				return backendTraceTestTx{msgs: []sdk.Msg{ethMsg}}, nil
			},
		}),
		queryClient: &rpctypes.QueryClient{QueryClient: query}, chainID: big.NewInt(1776),
	}
	block := &cmrpctypes.ResultBlock{Block: tmtypes.MakeBlock(height, []tmtypes.Tx{[]byte("encoded-cosmos-tx")}, nil, nil)}
	for attempt := 0; attempt < 2; attempt++ {
		got, err := b.TraceBlock(rpctypes.BlockNumber(height), config, block)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if len(got) != 1 || got[0] == nil || got[0].Error != "" || got[0].Result == nil {
			t.Fatalf("attempt %d returned cached rejection instead of success: %#v", attempt, got)
		}
		result := marshalMap(t, got[0].Result)
		if result["type"] != "CALL" || result["gasUsed"] != "0x5208" {
			t.Fatalf("attempt %d changed trace payload: %#v", attempt, result)
		}
	}
	if config.Timeout != "10000s" {
		t.Fatalf("caller config mutated to %q", config.Timeout)
	}
	if !reflect.DeepEqual(idx.blockReads, []string{"30s", "30s"}) || !reflect.DeepEqual(idx.blockWrites, []string{"30s"}) {
		t.Fatalf("cache did not use the effective timeout consistently: reads=%v writes=%v", idx.blockReads, idx.blockWrites)
	}
	gotCached, err := idx.TxIndexer.GetTraceBlockByHeight(height, &normalized)
	if err != nil || string(gotCached) != string(success) {
		t.Fatalf("normalized success not cached: %s, %v", gotCached, err)
	}
	gotOld, err := idx.TxIndexer.GetTraceBlockByHeight(height, config)
	if err != nil || string(gotOld) != string(oldFailure) {
		t.Fatalf("old cache entry should remain untouched: %s, %v", gotOld, err)
	}
	query.AssertExpectations(t)
}

func TestTraceTransactionNormalizesTimeoutBeforeCache(t *testing.T) {
	hash := common.HexToHash("0x1234")
	config := &rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: "callTracer", Timeout: "10000s"}}
	normalized := *config
	normalized.Timeout = "30s"
	idx := newTimeoutTraceIndexer(t)
	if err := idx.TxIndexer.SetTraceTransaction(hash, config, json.RawMessage(`{"type":"LEGACY"}`)); err != nil {
		t.Fatal(err)
	}
	if err := idx.TxIndexer.SetTraceTransaction(hash, &normalized, json.RawMessage(`{"type":"CALL","gasUsed":"0x5208"}`)); err != nil {
		t.Fatal(err)
	}
	b := &Backend{logger: backendTestLogger(), cfg: appconfig.Config{OfflineRPCOnly: true}, indexer: idx}
	got, err := b.TraceTransaction(hash, config)
	if err != nil {
		t.Fatalf("TraceTransaction: %v", err)
	}
	if result := marshalMap(t, got); result["type"] != "CALL" || result["gasUsed"] != "0x5208" {
		t.Fatalf("did not read normalized transaction cache: %#v", result)
	}
	if config.Timeout != "10000s" || !reflect.DeepEqual(idx.txReads, []string{"30s"}) {
		t.Fatalf("caller timeout=%q cache reads=%v", config.Timeout, idx.txReads)
	}
}

// Record cache access while retaining the production key encoding and storage.
type timeoutTraceIndexer struct {
	indexer.TxIndexer
	blockReads  []string
	blockWrites []string
	txReads     []string
}

func newTimeoutTraceIndexer(t *testing.T) *timeoutTraceIndexer {
	t.Helper()
	db := dbm.NewMemDB()
	t.Cleanup(func() { _ = db.Close() })
	return &timeoutTraceIndexer{TxIndexer: indexer.NewKVIndexer(db, backendTestLogger(), client.Context{})}
}

func (i *timeoutTraceIndexer) WithContext(context.Context) indexer.TxIndexer { return i }

func (i *timeoutTraceIndexer) GetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	i.blockReads = append(i.blockReads, config.Timeout)
	return i.TxIndexer.GetTraceBlockByHeight(height, config)
}

func (i *timeoutTraceIndexer) SetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig, raw json.RawMessage) error {
	i.blockWrites = append(i.blockWrites, config.Timeout)
	return i.TxIndexer.SetTraceBlockByHeight(height, config, raw)
}

func (i *timeoutTraceIndexer) GetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	i.txReads = append(i.txReads, config.Timeout)
	return i.TxIndexer.GetTraceTransaction(hash, config)
}
