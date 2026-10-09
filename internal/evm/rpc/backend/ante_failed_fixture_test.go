package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	cmrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/ethereum/go-ethereum/common"

	appconfig "github.com/InjectiveLabs/evm-gateway/internal/config"
	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	chaintypes "github.com/InjectiveLabs/sdk-go/chain/types"
)

const fixtureEVMChainID = "1776"

var errFixtureUnavailable = errors.New("fixture unavailable")

// fixtureComet serves mainnet fixture blocks and block results. Methods not
// overridden panic through the nil embedded client.
type fixtureComet struct {
	cmrpcclient.Client
	blocks     map[int64]*cmrpctypes.ResultBlock
	results    map[int64]*cmrpctypes.ResultBlockResults
	blockErr   error
	resultsErr error
	nilBlock   bool
}

func newFixtureComet(t *testing.T) *fixtureComet {
	t.Helper()
	c := &fixtureComet{
		blocks:  make(map[int64]*cmrpctypes.ResultBlock),
		results: make(map[int64]*cmrpctypes.ResultBlockResults),
	}
	for _, height := range mainnetfx.Heights {
		c.blocks[height] = mustFixtureResultBlock(t, height)
		c.results[height] = mustFixtureBlockResults(t, height)
	}
	return c
}

func (c *fixtureComet) Block(_ context.Context, height *int64) (*cmrpctypes.ResultBlock, error) {
	if c.blockErr != nil {
		return nil, c.blockErr
	}
	if c.nilBlock {
		return &cmrpctypes.ResultBlock{}, nil
	}
	if height == nil {
		return nil, errFixtureUnavailable
	}
	if block, ok := c.blocks[*height]; ok {
		return block, nil
	}
	return nil, errFixtureUnavailable
}

func (c *fixtureComet) BlockByHash(_ context.Context, hash []byte) (*cmrpctypes.ResultBlock, error) {
	for _, block := range c.blocks {
		if bytes.Equal(block.BlockID.Hash, hash) {
			return block, nil
		}
	}
	return nil, errFixtureUnavailable
}

func (c *fixtureComet) BlockResults(_ context.Context, height *int64) (*cmrpctypes.ResultBlockResults, error) {
	if c.resultsErr != nil {
		return nil, c.resultsErr
	}
	if height == nil {
		return nil, errFixtureUnavailable
	}
	if results, ok := c.results[*height]; ok {
		return results, nil
	}
	return nil, errFixtureUnavailable
}

func (c *fixtureComet) TxSearch(context.Context, string, bool, *int, *int, string) (*cmrpctypes.ResultTxSearch, error) {
	return nil, errFixtureUnavailable
}

type fixtureBackendOptions struct {
	virtual bool
	offline bool
	indexer txindexer.TxIndexer
	comet   *fixtureComet
	query   *rpcmocks.EVMQueryClient
}

func newFixtureBackend(t *testing.T, opts fixtureBackendOptions) *Backend {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	if opts.comet != nil {
		clientCtx = clientCtx.WithClient(opts.comet)
	}
	b := NewBackend(
		backendTestLogger(),
		appconfig.Config{
			EVMChainID:             fixtureEVMChainID,
			VirtualizeCosmosEvents: opts.virtual,
			OfflineRPCOnly:         opts.offline,
		},
		clientCtx,
		clientCtx,
		false,
		opts.indexer,
		nil,
	)
	query := opts.query
	if query == nil {
		query = &rpcmocks.EVMQueryClient{}
	}
	b.queryClient = &rpctypes.QueryClient{
		QueryClient:       query,
		TxFeesQueryClient: backendTestTxFeesQueryClient{err: errFixtureUnavailable},
	}
	return b
}

func newFixtureKV(t *testing.T, virtual bool) (*txindexer.KVIndexer, dbm.DB) {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	db := dbm.NewMemDB()
	kv := txindexer.NewKVIndexer(db, backendTestLogger(), clientCtx, txindexer.WithVirtualBankTransfers(virtual, fixtureEVMChainID))
	return kv, db
}

// newLegacyFixtureKV returns an indexer over the KV state written by the
// pre-fix indexer (earlier ante-failed inclusions own the re-included hashes).
func newLegacyFixtureKV(t *testing.T) (*txindexer.KVIndexer, dbm.DB) {
	t.Helper()
	kv, db := newFixtureKV(t, false)
	pairs, err := mainnetfx.LegacyReporterState()
	if err != nil {
		t.Fatalf("LegacyReporterState: %v", err)
	}
	for _, pair := range pairs {
		if err := db.Set(pair.Key, pair.Value); err != nil {
			t.Fatalf("seed legacy state: %v", err)
		}
	}
	return kv, db
}

func indexFixtureBlocks(t *testing.T, kv *txindexer.KVIndexer, heights ...int64) {
	t.Helper()
	for _, height := range heights {
		block, err := mainnetfx.Block(height)
		if err != nil {
			t.Fatalf("Block(%d): %v", height, err)
		}
		results := mustFixtureBlockResults(t, height)
		if _, err := kv.IndexBlockWithStatsAndResults(block, results); err != nil {
			t.Fatalf("index block %d: %v", height, err)
		}
	}
}

func mustFixtureResultBlock(t *testing.T, height int64) *cmrpctypes.ResultBlock {
	t.Helper()
	block, err := mainnetfx.ResultBlock(height)
	if err != nil {
		t.Fatalf("ResultBlock(%d): %v", height, err)
	}
	return block
}

func mustFixtureBlockResults(t *testing.T, height int64) *cmrpctypes.ResultBlockResults {
	t.Helper()
	results, err := mainnetfx.BlockResults(height)
	if err != nil {
		t.Fatalf("BlockResults(%d): %v", height, err)
	}
	return results
}

// fixtureEthMsgs decodes every MsgEthereumTx of a fixture block, ante-failed
// ones included, in block order.
func fixtureEthMsgs(t *testing.T, height int64) []*evmtypes.MsgEthereumTx {
	t.Helper()
	clientCtx, err := mainnetfx.ClientContext()
	if err != nil {
		t.Fatalf("ClientContext: %v", err)
	}
	block, err := mainnetfx.Block(height)
	if err != nil {
		t.Fatalf("Block(%d): %v", height, err)
	}
	var msgs []*evmtypes.MsgEthereumTx
	for _, txBz := range block.Txs {
		tx, err := clientCtx.TxConfig.TxDecoder()(txBz)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, msg := range tx.GetMsgs() {
			if ethMsg, ok := msg.(*evmtypes.MsgEthereumTx); ok {
				msgs = append(msgs, ethMsg)
			}
		}
	}
	return msgs
}

func msgHashes(msgs []*evmtypes.MsgEthereumTx) []common.Hash {
	hashes := make([]common.Hash, 0, len(msgs))
	for _, msg := range msgs {
		hashes = append(hashes, msg.Hash())
	}
	return hashes
}

func hashesEqual(got, want []common.Hash) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertHashList(t *testing.T, label string, got, want []common.Hash) {
	t.Helper()
	if !hashesEqual(got, want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
}

// overrideIndexer wraps an indexer and overrides selected lookups.
type overrideIndexer struct {
	txindexer.TxIndexer
	txs           map[common.Hash]*chaintypes.TxResult
	traceTxErr    error
	traceBlock    json.RawMessage
	traceBlockErr error
	visible       []common.Hash
	visibleErr    error
	virtual       map[common.Hash]bool
	virtualErr    error
}

func (o *overrideIndexer) WithContext(context.Context) txindexer.TxIndexer { return o }

func (o *overrideIndexer) GetByTxHash(hash common.Hash) (*chaintypes.TxResult, error) {
	if res, ok := o.txs[hash]; ok {
		return res, nil
	}
	if o.TxIndexer != nil {
		return o.TxIndexer.GetByTxHash(hash)
	}
	return nil, txindexer.ErrCacheMiss
}

func (o *overrideIndexer) GetTraceTransaction(hash common.Hash, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	if o.traceTxErr != nil || o.TxIndexer == nil {
		if o.traceTxErr != nil {
			return nil, o.traceTxErr
		}
		return nil, txindexer.ErrCacheMiss
	}
	return o.TxIndexer.GetTraceTransaction(hash, config)
}

func (o *overrideIndexer) GetTraceBlockByHeight(height int64, config *rpctypes.TraceConfig) (json.RawMessage, error) {
	if o.traceBlockErr != nil {
		return nil, o.traceBlockErr
	}
	if o.traceBlock != nil {
		return o.traceBlock, nil
	}
	if o.TxIndexer != nil {
		return o.TxIndexer.GetTraceBlockByHeight(height, config)
	}
	return nil, txindexer.ErrCacheMiss
}

func (o *overrideIndexer) GetRPCTransactionHashesByBlockHeight(height int64) ([]common.Hash, error) {
	if o.visibleErr != nil {
		return nil, o.visibleErr
	}
	if o.visible != nil {
		return o.visible, nil
	}
	if o.TxIndexer != nil {
		return o.TxIndexer.GetRPCTransactionHashesByBlockHeight(height)
	}
	return nil, nil
}

func (o *overrideIndexer) IsVirtualRPCTransaction(hash common.Hash) (bool, error) {
	if o.virtualErr != nil {
		return false, o.virtualErr
	}
	return o.virtual[hash], nil
}
