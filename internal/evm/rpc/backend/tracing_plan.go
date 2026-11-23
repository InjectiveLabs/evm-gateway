package backend

import (
	"fmt"

	evmtypes "github.com/InjectiveLabs/sdk-go/chain/evm/types"
	abci "github.com/cometbft/cometbft/abci/types"
	tmtypes "github.com/cometbft/cometbft/types"

	rpctypes "github.com/InjectiveLabs/evm-gateway/internal/evm/rpc/types"
)

// msgTraceError returns why message msgIndex of a Cosmos tx with msgCount
// messages and result res cannot be replayed, or "" when it can. It is the
// error of the message's synthetic failed frame.
//
// After a successful ante handler a Cosmos tx fails as a whole: baseapp runs
// its messages in order, stops at the first one that fails and discards the
// state of all of them. Only the failed message can be replayed; messages
// before it ran but were reverted, and messages after it never ran. A trace of
// either would report execution that had no effect on chain.
func msgTraceError(res *abci.ExecTxResult, msgIndex, msgCount int) string {
	failed, ok := rpctypes.FailedMsgIndex(res)
	switch {
	case !ok:
		return ""
	case failed < 0 && msgCount > 1:
		return "cosmos transaction failed: the failed message is unknown"
	case failed < 0, msgIndex == failed:
		return ""
	case msgIndex < failed:
		return fmt.Sprintf("reverted: message %d of the cosmos transaction failed", failed)
	default:
		return fmt.Sprintf("not executed: message %d of the cosmos transaction failed before it", failed)
	}
}

// traceMsg is a visible Ethereum message of a block. Messages with err are
// not replayed and get a synthetic failed frame with that error; alone
// messages are traced on their own, like debug_traceTransaction does.
type traceMsg struct {
	msg               *evmtypes.MsgEthereumTx
	txIndex, msgIndex int
	err               string
	alone             bool
}

// traceMsgs lists the visible Ethereum messages of block.Txs[:txLimit] in
// order. Replayed in sequence are the messages of Cosmos txs that succeeded,
// and failed messages that ran first in their Cosmos tx: replaying them on the
// block state reproduces the failure and changes nothing. A failed message
// that ran after other messages of its Cosmos tx is traced alone, on top of
// them, since their state must not be applied for the rest of the block.
// Without txResults every message is replayed. Txs that failed in the ante
// handler are not listed.
func (b *Backend) traceMsgs(block *tmtypes.Block, txResults []*abci.ExecTxResult, txLimit int) []traceMsg {
	if block == nil {
		return nil
	}
	if txLimit > len(block.Txs) {
		txLimit = len(block.Txs)
	}

	var list []traceMsg
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
		ethSeen := false
		for msgIndex, msg := range msgs {
			ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
			if !ok {
				continue
			}
			errMsg := msgTraceError(res, msgIndex, len(msgs))
			alone := errMsg == "" && ethSeen && res != nil && res.Code != abci.CodeTypeOK
			list = append(list, traceMsg{msg: ethMsg, txIndex: txIndex, msgIndex: msgIndex, err: errMsg, alone: alone})
			ethSeen = true
		}
	}
	return list
}

// replayedMsgs returns the messages of list replayed in sequence.
func replayedMsgs(list []traceMsg) []*evmtypes.MsgEthereumTx {
	var msgs []*evmtypes.MsgEthereumTx
	for _, m := range list {
		if m.err == "" && !m.alone {
			msgs = append(msgs, m.msg)
		}
	}
	return msgs
}
