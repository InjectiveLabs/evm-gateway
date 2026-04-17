# Operations

`evm-gateway` now has an explicit CLI surface and two distinct serving modes: the default online sync mode and an offline cache-backed mode.

## CLI

Build the binary:

```bash
go build ./cmd/evm-gateway
```

Useful commands:

```bash
./evm-gateway start
./evm-gateway version
./evm-gateway resync 12345678 12350000:12350025
```

`start` is also the default action when no subcommand is provided.

Configuration still comes from `WEB3INJ_*` environment variables, typically through `.env` or shell exports.

## Online Mode

The default mode starts live CometBFT and gRPC clients, runs the historical gap sync plus forward tip sync loop, and serves JSON-RPC from a cache-first backend. By default, `WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=true` starts the forward tip queue at startup head + 1 immediately, even while older indexed gaps are still being filled.

Typical configuration:

```bash
WEB3INJ_COMET_RPC=http://localhost:26657
WEB3INJ_GRPC_ADDR=127.0.0.1:9090
WEB3INJ_ENABLE_SYNC=true
WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=true
WEB3INJ_JSONRPC_ENABLE=true
```

In this mode the gateway prefers local KV reads for indexed heights and falls back to live chain queries on cache misses.

Set `WEB3INJ_PARALLEL_SYNC_TIP_AND_GAPS=false` to keep the legacy behavior: the gateway fills detected startup gaps first, then starts forward tip sync only after those gaps have completed or errored.

## Offline RPC-Only Mode

Offline mode starts JSON-RPC against the indexed KV store without constructing live CometBFT or gRPC clients.

Required settings:

```bash
WEB3INJ_CHAIN_ID=injective-1
WEB3INJ_ENABLE_SYNC=false
WEB3INJ_OFFLINE_RPC_ONLY=true
WEB3INJ_JSONRPC_ENABLE=true
```

Operational notes:

- The data dir must already contain indexed heights from a previous online sync or targeted `resync`.
- Requests are served only from indexed local data.
- `eth_blockNumber` reports the last indexed height.
- Cache-backed block, tx, receipt, log, and `eth_getBlockReceipts` paths continue to work for indexed heights.
- Debug trace methods only work when the required trace cache has already been warmed in a prior online run.

## Resync

Use `resync` to rewrite specific blocks or contiguous ranges in the local KV store without restarting a full historical backfill:

```bash
./evm-gateway resync 12000000 12000005:12000020
```

The command normalizes overlapping targets, deletes the cached data for those heights, re-fetches the blocks from the configured chain endpoints, and exits after the requested ranges are rebuilt.

## Sync Status

When JSON-RPC is enabled, the gateway exposes:

```text
GET /status/sync
```

The response includes:

- current phase and segment cursor
- earliest block, chain head, and last synced block
- gap counts
- cache hit, miss, and live-fallback counters for tx, receipt, and block-log lookups

This endpoint is used by the E2E parity and benchmark flows to confirm sync completion and detect unexpected live fallbacks.

## Telemetry

OpenTelemetry export is controlled by the `WEB3INJ_GOTRACER_*` variables in `.env.example`.

For a local plain OTLP gRPC receiver such as SigNoz on `:4317`, disable TLS explicitly:

```bash
WEB3INJ_GOTRACER_ENABLED=true
WEB3INJ_GOTRACER_COLLECTOR_DSN=localhost:4317
WEB3INJ_GOTRACER_COLLECTOR_ENABLE_TLS=false
```
