package backend

import (
	"fmt"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	abci "github.com/cometbft/cometbft/abci/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

// traceReplay says how a visible Ethereum message is traced.
type traceReplay int

const (
	// traceInBlock: replayed in sequence on the block state. These are the
	// messages of Cosmos txs that succeeded, and failed messages that were
	// the first Ethereum message of their Cosmos tx (replaying them on the
	// committed state reproduces the failure and changes nothing).
	traceInBlock traceReplay = iota
	// traceAlone: a failed message that executed after other Ethereum
	// messages of its Cosmos tx. It is traced on its own, with those messages
	// as predecessors, since their state was discarded with the Cosmos tx and
	// must not be applied for the rest of the block.
	traceAlone
	// traceNone: not traceable, see the entry's error.
	traceNone
)

// traceEntry is a visible Ethereum message of a block and how to trace it.
type traceEntry struct {
	msg      *evmtypes.MsgEthereumTx
	txIndex  int
	msgIndex int
	replay   traceReplay
	// sameTx holds the Ethereum messages that executed before msg in its
	// Cosmos tx (traceAlone only).
	sameTx []*evmtypes.MsgEthereumTx
	// err explains why the message is not traceable (traceNone only).
	err string
}

// msgTraceError returns why message msgIndex of a Cosmos tx with msgCount
// messages and result res cannot be traced, or "" when it can.
//
// After a successful ante handler a Cosmos tx fails as a whole: baseapp runs
// its messages in order, stops at the first one that fails and discards the
// state of all of them. Only the failed message can be replayed faithfully;
// messages before it ran but were reverted, and messages after it never ran.
// Tracing either of them would report execution that had no effect on chain.
func msgTraceError(hash common.Hash, res *abci.ExecTxResult, msgIndex, msgCount int) string {
	failed, ok := rpctypes.FailedMsgIndex(res)
	if !ok {
		return ""
	}
	switch {
	case failed < 0 && msgCount > 1:
		return fmt.Sprintf("transaction %s is not traceable: its Cosmos tx failed and the log does not name the failed message", hash.Hex())
	case failed < 0, msgIndex == failed:
		return ""
	case msgIndex < failed:
		return fmt.Sprintf("transaction %s was reverted: message %d of its Cosmos tx failed after it executed", hash.Hex(), failed)
	default:
		return fmt.Sprintf("transaction %s was not executed: message %d of its Cosmos tx failed before it", hash.Hex(), failed)
	}
}

// tracePlan lists the visible Ethereum messages of block.Txs[:txLimit] in
// order, with how to trace each. Without txResults every message is replayed
// in sequence. Txs that failed in the ante handler are not listed.
func (b *Backend) tracePlan(block *tmtypes.Block, txResults []*abci.ExecTxResult, txLimit int) []traceEntry {
	if block == nil {
		return nil
	}
	if txLimit > len(block.Txs) {
		txLimit = len(block.Txs)
	}

	var plan []traceEntry
	for txIndex, tx := range block.Txs[:txLimit] {
		var res *abci.ExecTxResult
		if txIndex < len(txResults) {
			res = txResults[txIndex]
		}
		if rpctypes.TxAnteFailed(res) {
			continue
		}

		decodedTx, err := b.clientCtx.TxConfig.TxDecoder()(tx)
		if err != nil {
			b.logger.Warn("failed to decode transaction in block", "height", block.Height, "error", err.Error())
			continue
		}

		msgs := decodedTx.GetMsgs()
		var executed []*evmtypes.MsgEthereumTx
		for msgIndex, msg := range msgs {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				continue
			}
			entry := traceEntry{msg: ethMsg, txIndex: txIndex, msgIndex: msgIndex}
			switch errMsg := msgTraceError(ethMsg.Hash(), res, msgIndex, len(msgs)); {
			case errMsg != "":
				entry.replay = traceNone
				entry.err = errMsg
			case len(executed) > 0 && res != nil && res.Code != abci.CodeTypeOK:
				entry.replay = traceAlone
				entry.sameTx = append([]*evmtypes.MsgEthereumTx(nil), executed...)
			default:
				entry.replay = traceInBlock
			}
			plan = append(plan, entry)
			executed = append(executed, ethMsg)
		}
	}
	return plan
}

// inBlockReplays returns the messages of plan replayed in sequence.
func inBlockReplays(plan []traceEntry) []*evmtypes.MsgEthereumTx {
	var msgs []*evmtypes.MsgEthereumTx
	for _, entry := range plan {
		if entry.replay == traceInBlock {
			msgs = append(msgs, entry.msg)
		}
	}
	return msgs
}
