// Package mainnetfx serves static Injective mainnet fixtures for tests.
//
// The fixtures cover the blocks of the "re-included tx" incident: Ethereum txs
// whose Cosmos tx failed in the ante handler (insufficient funds, gas used 0)
// and were included again in a later block with the same hash.
//
//   - block_<h>.json.gz, block_results_<h>.json.gz: Comet RPC /block and
//     /block_results results, captured from a mainnet node.
//   - sentry_trace_<h>.json.gz: debug_traceBlockByNumber(callTracer) results
//     served by the pre-fix evm-gateway deployment.
//   - legacy_kv_reporter_state.jsonl.gz: the complete KV state written by the
//     pre-fix indexer after indexing the later blocks first and the earlier
//     blocks second (the state the incident was reported against), as
//     [hex key, hex value] JSON lines.
package mainnetfx

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	cmtjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/ethereum/go-ethereum/common"

	chainclient "github.com/InjectiveLabs/sdk-go/client/chain"
)

//go:embed testdata/*.gz
var files embed.FS

const (
	// HeightEx1Failed holds two ante-failed txs from 0x1084…d3cd, one of them
	// TxEx1 (Cosmos tx #12), between two successful Ethereum txs.
	HeightEx1Failed int64 = 185577968
	// HeightEx1Included re-includes TxEx1, successfully.
	HeightEx1Included int64 = 185578125
	// HeightEx2Failed holds eight ante-failed txs (Cosmos txs #10-13, #15-18)
	// among four successful Ethereum txs.
	HeightEx2Failed int64 = 185579815
	// HeightEx2Included re-includes four of them (TxEx2 and co) which fail
	// after the ante handler (code 1, gas used 28209).
	HeightEx2Included int64 = 185579953

	// GasUsedEx2Included is the chain gas used of each re-included tx in
	// HeightEx2Included.
	GasUsedEx2Included uint64 = 28209
	// GasLimitAnteFailed is the gas limit of the ante-failed txs.
	GasLimitAnteFailed uint64 = 1200000
)

var (
	TxEx1        = common.HexToHash("0x03a649b3df8386840a76a77d26c156c7675f54ef02de1b786d89539f25eb8f5c")
	TxEx1Sibling = common.HexToHash("0x3da824b347259a6b8d26f6e1f754bef393cc415756804b487b0e79cf2af501f9")
	TxEx2        = common.HexToHash("0x4373888966789e7c58ce8dc096018f4122b2b21e8c8745eaf7f8258b941dfbba")

	// VisibleTxs lists the Ethereum txs each block exposes once ante-failed
	// txs are skipped, in order.
	VisibleTxs = map[int64][]common.Hash{
		HeightEx1Failed: {
			common.HexToHash("0xa61a00852036dbd7f88116056f7a62482682a531a089b88f6296cff1d6e438a0"),
			common.HexToHash("0xbedba8de19145b2bae9b5a3d56850af984d8cbc425a2df6107b731c104618aab"),
		},
		HeightEx1Included: {TxEx1},
		HeightEx2Failed: {
			common.HexToHash("0xa7e26f4562d81a1101e36da1c47e1b4e232a57f1dc13e41af1abafe3514acb49"),
			common.HexToHash("0xf2bbae7ed0836f7df432e73c624c9a4bb04621d0196d4e33d32ecbb092f8d97b"),
			common.HexToHash("0x881ad64f29f52f9e3fad962cb3bae1653bcdebffae6b25cc2a2f3a1c96dce17b"),
			common.HexToHash("0xe55c97835fb4a6e5a2ffae8efc3a83bad0379f79360bdd2e38cb11b57271f29e"),
		},
		HeightEx2Included: {
			TxEx2,
			common.HexToHash("0x1f79755c10e8aa8299a95e41acb57e7c73c1309466120235c186959df8baafbe"),
			common.HexToHash("0xd4c444c1431ba9c92a75f8cb5cb455e3b4c9ac622e82bdcee71bf7d8b5596bf9"),
			common.HexToHash("0x58abb9ca8bcb1017b4cf559cd15687bb0e848996005d523777f05af6ceb57dce"),
			common.HexToHash("0x3c293c659a5bb86373e9622a4e624ba0b528a2205eb69797964829ddbc448ec0"),
		},
	}

	// AnteFailedTxs lists the Ethereum txs of each block whose Cosmos tx failed
	// in the ante handler, in block order, with their Cosmos tx index.
	AnteFailedTxs = map[int64][]AnteFailedTx{
		HeightEx1Failed: {
			{TxIndex: 11, Hash: TxEx1Sibling},
			{TxIndex: 12, Hash: TxEx1},
		},
		HeightEx2Failed: {
			{TxIndex: 10, Hash: common.HexToHash("0x98b00005c1c7b72a6fbe5b41ccfddc088ba88e5018ff2b3f4da5234ca5cccda7")},
			{TxIndex: 11, Hash: common.HexToHash("0xf5e156651cb50510bd33773b7ebada79aaded835e317dd8c4c8d3f5732608099")},
			{TxIndex: 12, Hash: TxEx2},
			{TxIndex: 13, Hash: common.HexToHash("0x1f79755c10e8aa8299a95e41acb57e7c73c1309466120235c186959df8baafbe")},
			{TxIndex: 15, Hash: common.HexToHash("0x739d642231472715e63f219eac44780db8d0d3cf821600ad3a92cd5f17c36d3d")},
			{TxIndex: 16, Hash: common.HexToHash("0x0b42e37d1afd851af898040fb819d1e32f158337a13dbb5b39eb50503606ad91")},
			{TxIndex: 17, Hash: common.HexToHash("0xd4c444c1431ba9c92a75f8cb5cb455e3b4c9ac622e82bdcee71bf7d8b5596bf9")},
			{TxIndex: 18, Hash: common.HexToHash("0x58abb9ca8bcb1017b4cf559cd15687bb0e848996005d523777f05af6ceb57dce")},
		},
	}

	// Heights lists all fixture heights in ascending order.
	Heights = []int64{HeightEx1Failed, HeightEx1Included, HeightEx2Failed, HeightEx2Included}
)

// AnteFailedTx is an Ethereum tx whose Cosmos tx failed in the ante handler.
type AnteFailedTx struct {
	TxIndex uint32
	Hash    common.Hash
}

func readGzip(name string) ([]byte, error) {
	f, err := files.Open("testdata/" + name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// ResultBlock returns the Comet /block result at height.
func ResultBlock(height int64) (*coretypes.ResultBlock, error) {
	bz, err := readGzip(fmt.Sprintf("block_%d.json.gz", height))
	if err != nil {
		return nil, err
	}
	var res coretypes.ResultBlock
	if err := cmtjson.Unmarshal(bz, &res); err != nil {
		return nil, fmt.Errorf("decode block %d: %w", height, err)
	}
	return &res, nil
}

// Block returns the Comet block at height.
func Block(height int64) (*cmtypes.Block, error) {
	res, err := ResultBlock(height)
	if err != nil {
		return nil, err
	}
	return res.Block, nil
}

// BlockResults returns the Comet /block_results result at height.
func BlockResults(height int64) (*coretypes.ResultBlockResults, error) {
	bz, err := readGzip(fmt.Sprintf("block_results_%d.json.gz", height))
	if err != nil {
		return nil, err
	}
	var res coretypes.ResultBlockResults
	if err := cmtjson.Unmarshal(bz, &res); err != nil {
		return nil, fmt.Errorf("decode block results %d: %w", height, err)
	}
	return &res, nil
}

// SentryTrace returns the block trace served by the pre-fix deployment.
func SentryTrace(height int64) (json.RawMessage, error) {
	bz, err := readGzip(fmt.Sprintf("sentry_trace_%d.json.gz", height))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(bz), nil
}

// KV is a raw key/value pair.
type KV struct {
	Key   []byte
	Value []byte
}

// LegacyReporterState returns the KV state written by the pre-fix indexer.
func LegacyReporterState() ([]KV, error) {
	bz, err := readGzip("legacy_kv_reporter_state.jsonl.gz")
	if err != nil {
		return nil, err
	}
	out := make([]KV, 0)
	s := bufio.NewScanner(bytes.NewReader(bz))
	s.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for s.Scan() {
		var pair [2]string
		if err := json.Unmarshal(s.Bytes(), &pair); err != nil {
			return nil, err
		}
		key, err := hex.DecodeString(pair[0])
		if err != nil {
			return nil, err
		}
		value, err := hex.DecodeString(pair[1])
		if err != nil {
			return nil, err
		}
		out = append(out, KV{Key: key, Value: value})
	}
	return out, s.Err()
}

var (
	clientCtxOnce sync.Once
	clientCtx     client.Context
	clientCtxErr  error
)

// ClientContext returns a client context with the Injective codec and tx
// config, able to decode the fixture txs.
func ClientContext() (client.Context, error) {
	clientCtxOnce.Do(func() {
		clientCtx, clientCtxErr = chainclient.NewClientContext("", "", nil)
	})
	return clientCtx, clientCtxErr
}

// BlockResultsMap maps fixture heights to their block results.
type BlockResultsMap map[int64]*coretypes.ResultBlockResults

// AllBlockResults returns the block results of every fixture height.
func AllBlockResults() (BlockResultsMap, error) {
	out := make(BlockResultsMap, len(Heights))
	for _, height := range Heights {
		res, err := BlockResults(height)
		if err != nil {
			return nil, err
		}
		out[height] = res
	}
	return out, nil
}
