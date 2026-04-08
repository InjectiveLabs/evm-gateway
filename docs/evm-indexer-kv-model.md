# EVM KV Index Model

The gateway now stores the EVM data needed for cache-first JSON-RPC reads and selective offline serving out of its local KV store.

## Goals

- Persist the block, tx, receipt, log, and trace data required by the cache-backed RPC paths.
- Prefer local KV reads in normal online mode and fall back to live CometBFT or gRPC only on cache misses.
- Allow `WEB3INJ_OFFLINE_RPC_ONLY=true` to serve indexed data without creating live CometBFT or gRPC clients.
- Keep reindexing deterministic by deleting every cached collection for a height before rewriting that block.

## Collections

The implementation uses numeric key prefixes internally. The logical collections are:

- `meta/sync/*`
  - indexed range metadata, earliest and latest indexed heights, and last update time
- `tx/hash/{eth_tx_hash}`
  - canonical tx index record used to map Ethereum tx hashes back to the source Cosmos tx, msg index, and indexed EVM tx index
- `tx/num/{height}/{eth_tx_index}`
  - pointer from block height and EVM tx index to `tx/hash/{eth_tx_hash}`
- `rpc_tx/hash/{eth_tx_hash}`
  - prebuilt `RPCTransaction` payload for hash-based RPC reads
- `rpc_tx/index/{height}/{eth_tx_index}`
  - pointer from block height and EVM tx index to `rpc_tx/hash/{eth_tx_hash}`
- `receipt/hash/{eth_tx_hash}`
  - normalized Ethereum receipt payload, including status, cumulative gas used, logs, effective gas price, and contract address
- `block/logs/{height}`
  - grouped per-transaction logs for the block, used by cache-backed `eth_getLogs`
- `block/meta/{height}`
  - cached block summary including hash, parent hash, state root, miner, timestamp, size, gas limit, gas used, tx counts, bloom, transactions root, and base fee
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

## Resync Semantics

- `evm-gateway resync ...` normalizes the requested heights or ranges and fetches them again from the source chain.
- Before a height is rewritten, the gateway deletes the tx mappings, RPC tx payloads, receipts, block logs, block metadata, block-hash pointer, and any cached block or tx traces for that height.
- This keeps offline reads and debug traces consistent after targeted reindexing.
