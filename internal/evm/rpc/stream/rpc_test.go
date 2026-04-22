package stream

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

type fakeIndexedBlockReader struct {
	meta *txindexer.CachedBlockMeta
	logs [][]*virtualbank.RPCLog
	txs  []common.Hash
}

func (f fakeIndexedBlockReader) GetBlockMetaByHeight(int64) (*txindexer.CachedBlockMeta, error) {
	return f.meta, nil
}

func (f fakeIndexedBlockReader) GetLogsByBlockHeight(int64) ([][]*virtualbank.RPCLog, error) {
	return f.logs, nil
}

func (f fakeIndexedBlockReader) GetRPCTransactionHashesByBlockHeight(int64) ([]common.Hash, error) {
	return f.txs, nil
}

func TestPublishIndexedBlockReadsCachedHeaderAndLogs(t *testing.T) {
	blockHash := common.HexToHash("0x02")
	parentHash := common.HexToHash("0x01")
	stateRoot := common.HexToHash("0x03")
	transactionsRoot := common.HexToHash("0x04")
	miner := common.HexToAddress("0x0000000000000000000000000000000000000abc")
	baseFee := big.NewInt(123)
	logA := &virtualbank.RPCLog{
		Address:     virtualbank.ContractAddress,
		BlockNumber: 42,
		BlockHash:   blockHash,
		TxHash:      common.HexToHash("0x0a"),
		Index:       0,
		Virtual:     true,
	}
	logB := &virtualbank.RPCLog{
		Address:     common.HexToAddress("0x0000000000000000000000000000000000000def"),
		BlockNumber: 42,
		BlockHash:   blockHash,
		TxHash:      common.HexToHash("0x0b"),
		Index:       1,
	}

	stream := NewRPCStream(fakeIndexedBlockReader{
		meta: &txindexer.CachedBlockMeta{
			Height:           42,
			Hash:             blockHash.Hex(),
			ParentHash:       parentHash.Hex(),
			StateRoot:        stateRoot.Hex(),
			Miner:            miner.Hex(),
			Timestamp:        1000,
			GasLimit:         30_000_000,
			GasUsed:          21000,
			EthTxCount:       2,
			Bloom:            hexutil.Encode(make([]byte, ethtypes.BloomByteLength)),
			TransactionsRoot: transactionsRoot.Hex(),
			BaseFee:          hexutil.EncodeBig(baseFee),
		},
		logs: [][]*virtualbank.RPCLog{{logA}, {logB}},
		txs:  []common.Hash{logA.TxHash, logB.TxHash},
	})

	if err := stream.PublishIndexedBlock(42); err != nil {
		t.Fatalf("PublishIndexedBlock returned error: %v", err)
	}

	headers, _ := stream.HeaderStream().ReadAllNonBlocking(0)
	if len(headers) != 1 {
		t.Fatalf("unexpected header count: got %d want 1", len(headers))
	}
	if headers[0].Hash != blockHash {
		t.Fatalf("unexpected block hash: got %s want %s", headers[0].Hash, blockHash)
	}
	header := headers[0].EthHeader
	if header.Number.Int64() != 42 || header.ParentHash != parentHash || header.Root != stateRoot {
		t.Fatalf("unexpected header: %#v", header)
	}
	if header.Coinbase != miner || header.TxHash != transactionsRoot || header.GasUsed != 21000 {
		t.Fatalf("unexpected cached header fields: %#v", header)
	}
	if header.BaseFee == nil || header.BaseFee.Cmp(baseFee) != 0 {
		t.Fatalf("unexpected base fee: %v", header.BaseFee)
	}

	logs, _ := stream.LogStream().ReadAllNonBlocking(0)
	if len(logs) != 2 {
		t.Fatalf("unexpected log count: got %d want 2", len(logs))
	}
	if logs[0] != logA || logs[1] != logB {
		t.Fatalf("logs were not published in stored order")
	}
	if !logs[0].Virtual {
		t.Fatal("expected virtual log metadata to be preserved")
	}

	pending, _ := stream.PendingTxStream().ReadAllNonBlocking(0)
	if len(pending) != 2 {
		t.Fatalf("unexpected pending tx hash count: got %d want 2", len(pending))
	}
	if pending[0] != logA.TxHash || pending[1] != logB.TxHash {
		t.Fatalf("tx hashes were not published in stored order")
	}
}
