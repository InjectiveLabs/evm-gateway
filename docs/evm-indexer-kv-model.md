# EVM KV Index Model

The gateway now stores the EVM data needed for cache-first JSON-RPC reads and selective offline serving out of its local KV store.

When `WEB3INJ_VIRTUALIZE_COSMOS_EVENTS=true`, the same KV model also stores virtual JSON-RPC transactions and logs synthesized from native Cosmos events. The source of truth is still CometBFT block results; the virtual records are a JSON-RPC projection over those events.

## Goals

- Persist the block, tx, receipt, log, and trace data required by the cache-backed RPC paths.
- Prefer local KV reads in normal online mode and fall back to live CometBFT or gRPC only on cache misses.
- Keep the forward tip index current during large historical backfills when `WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=true` (the default).
- Allow `WEB3INJ_OFFLINE_RPC_ONLY=true` to serve indexed data without creating live CometBFT or gRPC clients.
- Keep reindexing deterministic by deleting every cached collection for a height before rewriting that block.
- Optionally expose tracked Cosmos `x/bank` events as virtual Ethereum transactions and logs without changing historical EVM-only behavior when the feature flag is disabled.

## Collections

The implementation uses numeric key prefixes internally. The logical collections are:

- `meta/sync/*`
  - indexed range metadata, earliest and latest indexed heights, and last update time
- `tx/hash/{eth_tx_hash}`
  - canonical tx index record used to map Ethereum tx hashes back to the source Cosmos tx, msg index, and indexed EVM tx index
- `tx/num/{height}/{eth_tx_index}`
  - pointer from block height and EVM tx index to `tx/hash/{eth_tx_hash}`
- `rpc_tx/hash/{eth_tx_hash}`
  - prebuilt `RPCTransaction` payload for hash-based RPC reads; this also stores virtual RPC transactions when Cosmos event virtualization is enabled
- `rpc_tx/index/{height}/{eth_tx_index}`
  - pointer from block height and EVM tx index to `rpc_tx/hash/{eth_tx_hash}`
- `rpc_tx/virtual/{virtual_tx_hash}`
  - marker used to identify virtual RPC transactions so they can be hidden when a caller is using a gateway instance without Cosmos event virtualization enabled
- `receipt/hash/{eth_tx_hash}`
  - normalized Ethereum receipt payload, including status, cumulative gas used, logs, effective gas price, and contract address; virtual receipts use zero gas price/value defaults and `to = 0x0000000000000000000000000000000000000800`
- `block/logs/{height}`
  - grouped per-transaction logs for the block, used by cache-backed `eth_getLogs`; virtualized mode stores `RPCLog` entries so virtual logs can carry `virtual` and `cosmos_hash` JSON metadata
- `block/meta/{height}`
  - cached block summary including hash, parent hash, state root, miner, timestamp, size, gas limit, gas used, tx counts, bloom, transactions root, base fee, and the `virtualized_cosmos_events` mode used when the block was indexed
- `block/hash/{block_hash}`
  - pointer from block hash to indexed height
- `trace/tx/{eth_tx_hash}/{trace_config_hash}`
  - cached `debug_traceTransaction` result for a specific trace config
- `trace/block/{height}/{trace_config_hash}`
  - cached `debug_traceBlockByNumber` or `debug_traceBlockByHash` result for a specific trace config

## Query Routing

- In normal online mode, block, tx, receipt, log, and trace requests attempt KV reads first and only fall back to live endpoints when the requested range is not cached.
- In offline RPC-only mode, the gateway does not create live CometBFT or gRPC clients. Cache misses return `nil` for ordinary read paths or an explicit trace error when the required trace cache was never warmed.
- `eth_blockNumber` in offline mode is derived from the last indexed height.
- `debug_traceTransaction` can reuse a cached block trace when a dedicated tx trace entry is missing but the block trace for that config already exists.
- Cache reads are mode-aware. If a block was indexed with a different Cosmos event virtualization setting than the current gateway process, online mode falls back to live reconstruction and offline mode returns a mode-mismatch error for affected paths.
- When Cosmos event virtualization is enabled, `eth_getLogs`, block queries, receipt queries, block receipts, and transaction lookup paths return the virtualized block view.
- Live Comet event streams are disabled for log filters in virtualized mode. The existing event stream only carries native EVM logs and cannot produce complete begin/end blocker logs or stable virtual log ordering.

## Cosmos Event Virtualization

Virtualization is controlled by `WEB3INJ_VIRTUALIZE_COSMOS_EVENTS`.

The gateway tracks these Cosmos event types from tx results and finalize block events:

- `transfer`
- `coin_spent`
- `coin_received`
- `coinbase`
- `burn`

The Solidity ABI for generated topics is `contracts/InjectiveNativeBankTransfers.sol`. All virtual logs use reserved pseudo-contract address `0x0000000000000000000000000000000000000800`. Cosmos address fields are encoded as right-aligned `bytes32` topics so both 20-byte EVM addresses and longer Cosmos addresses fit the ABI.

Virtual transaction rules:

- EVM transactions keep their real transaction hash. Any bank side-effect logs from the same `MsgEthereumTx` are appended after that transaction's native EVM logs.
- Non-EVM Cosmos transactions with tracked events get one virtual RPC transaction. Its hash is `keccak256(cosmos_tx_hash)`, and its JSON includes `virtual: true` and `cosmos_hash`.
- Finalize block events are split by their `mode` attribute. `mode=BeginBlock` events go into the begin-block virtual transaction. All other tracked finalize events go into the end-block virtual transaction.
- Begin-block and end-block virtual transaction hashes are deterministic hashes of the phase name and height. They include `virtual: true` but no `cosmos_hash`.
- Virtual transactions use empty input, zero gas/value defaults, legacy tx type, and `to = 0x0000000000000000000000000000000000000800`.

Block ordering in virtualized mode is:

```text
[begin_block_virtual_tx] [normal EVM txs and virtualized Cosmos txs in block order] [end_block_virtual_tx]
```

Log ordering follows the same block order. For EVM transactions with native bank side effects, real EVM logs remain first and virtual bank logs are appended after them.

## Sync Semantics

- On startup, the indexer loads indexed ranges and computes gaps between `WEB3INJ_EARLIEST_BLOCK` and the current CometBFT head.
- With `WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=true`, historical gaps are filled in one queue while a forward tip queue starts immediately at startup head + 1.
- With `WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=false`, the gateway keeps the legacy ordering: fill or error all startup gaps first, then start forward tip sync.
- If either queue stops on a non-cancellation error, the other queue continues. If both queues stop, the gateway keeps serving JSON-RPC from existing indexed data and live fallback paths.

## Resync Semantics

- `evm-gateway resync ...` normalizes the requested heights or ranges and fetches them again from the source chain.
- Before a height is rewritten, the gateway deletes the tx mappings, RPC tx payloads, receipts, block logs, block metadata, block-hash pointer, and any cached block or tx traces for that height.
- This keeps offline reads and debug traces consistent after targeted reindexing.
