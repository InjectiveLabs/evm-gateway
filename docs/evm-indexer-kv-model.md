# EVM-Only KV Index Model

This model stores all EVM-relevant data from each block and excludes non-EVM Cosmos payloads.

## Goals
- Persist 100% of EVM transaction and log/event data per block.
- Serve tx lookups, receipt/log queries, and block-level EVM iterations from local KV cache.
- Fall back to live chain only when block range is not indexed yet.
- Keep Cosmos-only messages/events out of storage.

## Collections
- `meta/sync/*`
  - `meta/sync/earliest_indexed`: int64
  - `meta/sync/latest_indexed`: int64
  - `meta/sync/ranges`: compact serialized range list
  - `meta/sync/updated_at`: timestamp
- `blk/{height}`
  - EVM block summary:
  - block hash, parent hash, timestamp, bloom, gas used, gas limit, base fee
  - tx count, first/last EVM tx index
- `tx/hash/{eth_tx_hash}`
  - canonical tx record:
  - block height/hash, eth tx index, cosmos tx index/msg index
  - from/to, nonce, value, gas, gas price/eip1559 fields, input
  - status, gas used, cumulative gas used, contract address, type
- `tx/num/{height}/{eth_tx_index}`
  - pointer to `eth_tx_hash`
- `rcpt/hash/{eth_tx_hash}`
  - receipt payload:
  - status, cumulative gas used, logs bloom, effective gas price
  - logs references or embedded logs
- `log/{height}/{eth_tx_index}/{log_index}`
  - normalized log payload:
  - address, topics[0..n], data, removed=false
- `log/topic/{topic}/{height}/{eth_tx_index}/{log_index}`
  - pointer to `log/...`
- `log/address/{address}/{height}/{eth_tx_index}/{log_index}`
  - pointer to `log/...`
- `log/address_topic/{address}/{topic}/{height}/{eth_tx_index}/{log_index}`
  - pointer to `log/...`

## Query Routing
- If requested block/tx range is fully indexed:
  - serve from local KV only.
- If partially or not indexed:
  - live-query chain RPC and track miss/fallback metrics.

## Notes
- Current implementation already stores EVM tx index mappings.
- Next increment should add dedicated block/receipt/log collections above and route `eth_getLogs` and receipt-heavy paths to cache-first execution.
