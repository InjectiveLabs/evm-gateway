package stream

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/virtualbank"
	txindexer "github.com/InjectiveLabs/evm-gateway/internal/indexer"
)

const (
	headerStreamSegmentSize = 128
	headerStreamCapacity    = 128 * 32
	txStreamSegmentSize     = 1024
	txStreamCapacity        = 1024 * 32
	logStreamSegmentSize    = 2048
	logStreamCapacity       = 2048 * 32
)

type IndexedBlockReader interface {
	GetBlockMetaByHeight(height int64) (*txindexer.CachedBlockMeta, error)
	GetLogsByBlockHeight(height int64) ([][]*virtualbank.RPCLog, error)
	GetRPCTransactionHashesByBlockHeight(height int64) ([]common.Hash, error)
}

type RPCHeader struct {
	EthHeader *ethtypes.Header
	Hash      common.Hash
}

// RPCStream provides data streams for newHeads, logs, and newPendingTransactions.
// Streams are published only after the indexer has committed the
// enriched block view to KV, so websocket clients observe the same virtualized
// data that polling RPC methods read.
type RPCStream struct {
	reader IndexedBlockReader

	headerStream    *Stream[RPCHeader]
	pendingTxStream *Stream[common.Hash]
	logStream       *Stream[*virtualbank.RPCLog]
}

func NewRPCStream(reader IndexedBlockReader) *RPCStream {
	return &RPCStream{
		reader: reader,

		headerStream:    NewStream[RPCHeader](headerStreamSegmentSize, headerStreamCapacity),
		pendingTxStream: NewStream[common.Hash](txStreamSegmentSize, txStreamCapacity),
		logStream:       NewStream[*virtualbank.RPCLog](logStreamSegmentSize, logStreamCapacity),
	}
}

func (s *RPCStream) Close() error {
	return nil
}

func (s *RPCStream) HeaderStream() *Stream[RPCHeader] {
	return s.headerStream
}

func (s *RPCStream) PendingTxStream() *Stream[common.Hash] {
	return s.pendingTxStream
}

func (s *RPCStream) LogStream() *Stream[*virtualbank.RPCLog] {
	return s.logStream
}

// ListenPendingTx publishes a transaction hash to the pending transaction
// compatibility stream.
func (s *RPCStream) ListenPendingTx(hash common.Hash) {
	if s == nil {
		return
	}
	s.pendingTxStream.Add(hash)
}

// PublishIndexedBlock broadcasts the cached header and logs for a block that
// the forward tip sync queue has just written to KV.
func (s *RPCStream) PublishIndexedBlock(height int64) error {
	if s == nil || s.reader == nil {
		return nil
	}

	meta, err := s.reader.GetBlockMetaByHeight(height)
	if err != nil {
		return fmt.Errorf("load indexed block meta %d: %w", height, err)
	}
	header, err := txindexer.HeaderFromCachedBlockMeta(meta)
	if err != nil {
		return err
	}
	logGroups, err := s.reader.GetLogsByBlockHeight(height)
	if err != nil {
		return fmt.Errorf("load indexed block logs %d: %w", height, err)
	}
	txHashes, err := s.reader.GetRPCTransactionHashesByBlockHeight(height)
	if err != nil {
		return fmt.Errorf("load indexed block tx hashes %d: %w", height, err)
	}

	s.headerStream.Add(RPCHeader{
		EthHeader: header,
		Hash:      common.HexToHash(meta.Hash),
	})
	s.pendingTxStream.Add(txHashes...)
	s.logStream.Add(flattenRPCLogs(logGroups)...)
	return nil
}

func flattenRPCLogs(groups [][]*virtualbank.RPCLog) []*virtualbank.RPCLog {
	out := make([]*virtualbank.RPCLog, 0)
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}
