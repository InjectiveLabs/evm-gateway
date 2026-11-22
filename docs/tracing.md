# Debug traces

`debug_traceBlockByNumber` and `debug_traceBlockByHash` return one entry per transaction of the block, in the order `eth_getBlockByNumber` lists them. Each entry carries the tx hash:

```json
{"txHash": "0x…", "result": { … }}
```

Indexers can match entries to transactions by position or by `txHash`. `debug_traceTransaction` returns the same `result` as the tx's block entry.

## Which transactions are replayed

The chain replays the Ethereum transactions that changed its state:

- Ethereum txs of Cosmos txs that succeeded;
- the failed Ethereum message of a failed Cosmos tx. It is replayed in the block when it ran first in its Cosmos tx. Otherwise it is replayed on its own after the messages that ran before it, as `debug_traceTransaction` does.

Their results come from the requested tracer.

Txs that failed in the ante handler are not visible over JSON-RPC (no nonce consumed), so they have no entry and `debug_traceTransaction` does not find them.

## Synthetic results

Some visible transactions cannot be replayed:

- **Virtual txs** (`WEB3INJ_VIRTUALIZE_COSMOS_EVENTS=true`): native Cosmos events, such as x/bank transfers and IBC hook calls, exposed as transactions. They did not run in the EVM.
- **Messages of a failed Cosmos tx that had no effect.** After its ante handler, a Cosmos tx runs its messages in order, stops at the first one that fails and reverts all of them. Messages before the failed one were reverted, and messages after it never ran.
- **Visible Ethereum txs missing from the replay**, which should not happen.

These get a result built from the transaction and receipt that `eth_getTransactionByHash` and `eth_getTransactionReceipt` return. It has the shape of the requested tracer:

| Tracer | Result |
|---|---|
| `callTracer` | One top-level frame without inner calls: `type` `CALL` (`CREATE` for a tx without `to`), `from`, `to`, `value`, `gas` (the tx gas), `gasUsed` (the receipt's), `input`. Failed txs add `error` (and `revertReason` when the receipt has one). With `withLog`, successful frames include the receipt logs at `position` `0x0`. |
| struct logger (no tracer) | `{"gas": <gasUsed>, "failed": <bool>, "returnValue": "0x", "structLogs": []}` |
| `prestateTracer` | `{}`, or `{"pre": {}, "post": {}}` with `diffMode` |
| other tracers | `null` |

A frame's `error` is:

| Transaction | `error` |
|---|---|
| failed virtual tx | the receipt's `vmError`, otherwise `cosmos transaction failed` |
| message after the failed message `N` | `not executed: message N of the cosmos transaction failed before it` |
| message before `N` | `reverted: message N of the cosmos transaction failed` |
| message of a multi-message tx whose log names no message | `cosmos transaction failed: the failed message is unknown` |
| visible Ethereum tx missing from the replay | `transaction trace unavailable` |

An entry-level `error` (`{"txHash": "0x…", "error": "…"}`) means that tracing failed, for example on a timeout. As in geth, the tx has no result.

## Caching

Block traces are cached per height and tracer configuration. Each read aligns the cached entries with the visible transactions of the block. This also rebuilds the `{"type": 0}` placeholders that v1.4.3 and v1.4.4 wrote for virtual txs, so caches written by those versions need no resync.
