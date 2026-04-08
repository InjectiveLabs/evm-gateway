# EVM Gateway Architecture

## Overview
`evm-gateway` is a standalone Ethereum JSON-RPC server for Injective EVM. It runs either in normal online mode (live CometBFT + gRPC, cache-first RPC) or offline RPC-only mode (serve indexed KV data only). It exposes HTTP on `:8545`, WS on `:8546`, and a sync status endpoint on `/status/sync`.

## Key Components
- `cmd/evm-gateway`: `mow.cli` entrypoint. Default action is `start`; also has `version` and `resync`.
- `internal/config`: Plain env-file loader with `WEB3INJ_` prefix. `Config` is passed through the app without Cosmos `server.Context` or viper.
- `internal/app`: Orchestrates lifecycle. Builds live clients unless `WEB3INJ_OFFLINE_RPC_ONLY=true`, opens the indexer DB, runs sync/resync, starts JSON-RPC, and handles shutdown.
- `internal/jsonrpc`: HTTP + WS server wiring. Registers EVM namespaces, exposes `/status/sync`, and degrades to polling-only mode if Comet event streams are unavailable.
- `internal/evm/rpc`: Port of Injective’s RPC implementation. Cache-first block/tx/receipt/log paths, `eth_getBlockReceipts`, and trace fallback/caching live here.
- `internal/indexer`: KV indexer and syncer. Stores tx mappings, RPC tx payloads, receipts, block metadata, grouped block logs, and cached trace results; `resync` rewrites selected heights.
- `internal/blocksync`: Parallel block fetcher (ported from coremon) plus pace logging; used by the indexer sync loop.
- `internal/telemetry`: OpenTelemetry wiring via `gotracer`.

## Indexer Flow
1. Load indexed ranges from the KV DB (`evmindexer` in `DATA_DIR/data`).
2. Compute gaps between `EARLIEST_BLOCK` and the current chain head.
3. Fill gaps in order, then switch to forward-only syncing from head+1.
4. `resync` skips forward sync, rewrites only requested ranges, and exits.

## Configuration
All configuration is driven by env vars with the `WEB3INJ_` prefix. Defaults are captured in `.env.example`. Critical inputs:
- `WEB3INJ_COMET_RPC` and `WEB3INJ_GRPC_ADDR` (ABCI and gRPC endpoints)
- `WEB3INJ_EARLIEST_BLOCK` (minimum height to backfill)
- `WEB3INJ_FETCH_JOBS` (parallel block fetchers)
- `WEB3INJ_CHAIN_ID` (validated online; required for offline RPC-only mode)
- `WEB3INJ_ENABLE_SYNC` and `WEB3INJ_OFFLINE_RPC_ONLY`
- `WEB3INJ_GOTRACER_*` for OTEL export

## Logging
Uses `log/slog` exclusively (JSON or text), configured via env/flags.
