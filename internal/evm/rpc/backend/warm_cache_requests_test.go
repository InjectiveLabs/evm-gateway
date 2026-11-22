package backend

import (
	"fmt"
	"testing"

	txfeestypes "github.com/InjectiveLabs/sdk-go/chain/txfees/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	rpcmocks "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/backend/mocks"
	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
	"github.com/InjectiveLabs/evm-gateway/internal/testutil/mainnetfx"
)

// panicTxFeesQueryClient panics on any request through the nil embedded client.
type panicTxFeesQueryClient struct{ txfeestypes.QueryClient }

// TestWarmCacheServesReadsWithoutBackendRequests checks that, in online mode,
// reads of indexed heights are answered from the KV cache alone: no Comet
// request and no gRPC query (an unexpected gRPC query panics).
func TestWarmCacheServesReadsWithoutBackendRequests(t *testing.T) {
	heights := []int64{mainnetfx.HeightEx2Failed, mainnetfx.HeightMultiMsgFailure}
	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprintf("virtual=%v", virtual), func(t *testing.T) {
			kv, _ := newFixtureKV(t, virtual)
			indexFixtureBlocks(t, kv, heights...)
			comet := newFixtureComet(t)
			b := newFixtureBackend(t, fixtureBackendOptions{
				virtual: virtual,
				indexer: kv,
				comet:   comet,
				query:   &rpcmocks.EVMQueryClient{},
			})
			b.queryClient.TxFeesQueryClient = panicTxFeesQueryClient{}

			for _, height := range heights {
				num := rpctypes.BlockNumber(height)
				hash := common.BytesToHash(comet.blocks[height].BlockID.Hash)
				block, err := b.GetBlockByNumber(num, true)
				if err != nil || block == nil {
					t.Fatalf("GetBlockByNumber(%d): %v %v", height, block, err)
				}
				if _, err := b.GetBlockByNumber(num, false); err != nil {
					t.Fatalf("GetBlockByNumber(%d, false): %v", height, err)
				}
				if _, err := b.GetBlockByHash(hash, true); err != nil {
					t.Fatalf("GetBlockByHash(%d): %v", height, err)
				}
				if _, err := b.HeaderByNumber(num); err != nil {
					t.Fatalf("HeaderByNumber(%d): %v", height, err)
				}
				if _, err := b.HeaderByHash(hash); err != nil {
					t.Fatalf("HeaderByHash(%d): %v", height, err)
				}
				count := b.GetBlockTransactionCountByNumber(num)
				if count == nil {
					t.Fatalf("GetBlockTransactionCountByNumber(%d): nil", height)
				}
				if b.GetBlockTransactionCountByHash(hash) == nil {
					t.Fatalf("GetBlockTransactionCountByHash(%d): nil", height)
				}
				if _, err := b.GetBlockReceipts(rpctypes.BlockNumberOrHash{BlockNumber: &num}); err != nil {
					t.Fatalf("GetBlockReceipts(%d): %v", height, err)
				}
				if _, err := b.GetLogs(hash); err != nil {
					t.Fatalf("GetLogs(%d): %v", height, err)
				}
				if _, err := b.GetLogsByHeight(&height); err != nil {
					t.Fatalf("GetLogsByHeight(%d): %v", height, err)
				}
				for i := 0; i < int(*count); i++ {
					tx, err := b.GetTransactionByBlockNumberAndIndex(num, hexutil.Uint(i))
					if err != nil || tx == nil {
						t.Fatalf("GetTransactionByBlockNumberAndIndex(%d, %d): %v %v", height, i, tx, err)
					}
					if _, err := b.GetTransactionByBlockHashAndIndex(hash, hexutil.Uint(i)); err != nil {
						t.Fatalf("GetTransactionByBlockHashAndIndex(%d, %d): %v", height, i, err)
					}
					if _, err := b.GetTransactionByHash(tx.Hash); err != nil {
						t.Fatalf("GetTransactionByHash(%s): %v", tx.Hash.Hex(), err)
					}
					receipt, err := b.GetTransactionReceipt(tx.Hash)
					if err != nil || receipt == nil {
						t.Fatalf("GetTransactionReceipt(%s): %v %v", tx.Hash.Hex(), receipt, err)
					}
				}
			}

			if len(comet.calls) != 0 {
				t.Fatalf("warm cache made comet requests: %v", comet.calls)
			}

			for _, height := range heights {
				// hidden ante-failed txs resolve to null
				for _, hidden := range mainnetfx.AnteFailedTxs[height] {
					if tx, err := b.GetTransactionByHash(hidden.Hash); err != nil || tx != nil {
						t.Fatalf("ante-failed GetTransactionByHash(%s): %v %v", hidden.Hash.Hex(), tx, err)
					}
					if receipt, err := b.GetTransactionReceipt(hidden.Hash); err != nil || receipt != nil {
						t.Fatalf("ante-failed GetTransactionReceipt(%s): %v %v", hidden.Hash.Hex(), receipt, err)
					}
				}
			}
			// Hidden txs are not indexed, so their lookups miss the cache and
			// fall back to the Comet tx index and the mempool like any unknown
			// hash: two TxSearch requests and one UnconfirmedTxs per hash.
			hidden := 0
			for _, height := range heights {
				hidden += len(mainnetfx.AnteFailedTxs[height])
			}
			if comet.calls["TxSearch"] != 2*hidden || comet.calls["UnconfirmedTxs"] != hidden || len(comet.calls) != 2 {
				t.Fatalf("hidden tx lookups made comet requests %v for %d hashes", comet.calls, hidden)
			}
		})
	}
}
