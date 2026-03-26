# EVM Gateway Architecture

## Overview
evm-gateway is a standalone JSON-RPC server for Injective EVM that mirrors the in-node RPC implementation but runs as a separate process with its own config, flags, and lifecycle. It connects to a CometBFT RPC endpoint for block data and to gRPC for module queries, builds its own Cosmos client context, and serves Ethereum JSON-RPC over HTTP (8545) and WS (8546).

## Key Components
- `cmd/evm-gateway/main.go`: CLI entrypoint. Loads `.env`/`WEB3INJ_` config, applies flags, initializes logging/telemetry, and starts the app.
- `internal/config`: Plain env-file loader with `WEB3INJ_` prefix. `Config` is passed through the app without Cosmos `server.Context` or viper.
- `internal/app`: Orchestrates lifecycle. Builds client context (CometBFT RPC + gRPC), opens the indexer DB, starts the indexer sync loop and JSON-RPC servers, and performs graceful shutdown.
- `internal/jsonrpc`: HTTP + WS server wiring. Registers EVM namespaces and bridges Ethereum logging to `log/slog`.
- `internal/evm/rpc`: Port of Injective’s RPC implementation (backend + namespaces) using `slog` and the local config struct.
- `internal/indexer`: KV indexer for EVM tx results and the syncer that fills historical gaps before switching to forward-only indexing.
- `internal/blocksync`: Parallel block fetcher (ported from coremon) plus pace logging; used by the indexer sync loop.
- `internal/telemetry`: Statsd and OpenTelemetry (gotracer) wiring.

## Indexer Flow
1. Load indexed ranges from the KV DB (`evmindexer` in `DATA_DIR/data`).
2. Compute gaps between `EARLIEST_BLOCK` and the current chain head.
3. Fill gaps in order, then switch to forward-only syncing from head+1.
4. Pace logging reports sync throughput.

## Configuration
All configuration is driven by env vars with the `WEB3INJ_` prefix. Defaults are captured in `.env.example`. Critical inputs:
- `WEB3INJ_COMET_RPC` and `WEB3INJ_GRPC_ADDR` (ABCI and gRPC endpoints)
- `WEB3INJ_EARLIEST_BLOCK` (minimum height to backfill)
- `WEB3INJ_FETCH_JOBS` (parallel block fetchers)
- `WEB3INJ_CHAIN_ID` (optional, validated against the RPC endpoint)

## Logging
Uses `log/slog` exclusively (JSON or text), configured via env/flags.
