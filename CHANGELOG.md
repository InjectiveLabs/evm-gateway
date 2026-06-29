<!--
Guiding Principles:

Changelogs are for humans, not machines.
There should be an entry for every single version.
The same types of changes should be grouped.
Versions and sections should be linkable.
The latest version comes first.
The release date of each version is displayed.

Usage:

Change log entries are to be added to the Unreleased section under the
appropriate stanza (see below). Each entry is required to include a tag and
the Github PR reference in the following format:

* (<tag>) \#<pr-number> message

The tag should consist of where the change is being made ex. (exchange), (iavl), (rpc)
The PR numbers must be later be link-ified during the release process so you do
not have to worry about including a link manually, but you can if you wish.

Types of changes (Stanzas):

"Features" for new features.
"Improvements" for changes in existing functionality and performance improvements.
"Deprecated" for soon-to-be removed features.
"Bug Fixes" for any bug fixes, except security related.
"Security" for security related changes and exploit fixes. NOT EXPORTED in auto-publishing process.
"API Breaking" for breaking Protobuf, gRPC and REST routes and types used by end-users.
"CLI Breaking" for breaking CLI commands.
Ref: https://keepachangelog.com/en/1.1.0/
-->

# Changelog

## [v1.4.2] - 2026-06-30

### Features

* (rpc) Added `WEB3INJ_COMET_BROADCAST_RPC` to route transaction broadcasts through a separate CometBFT endpoint while keeping sync and read traffic on `WEB3INJ_COMET_RPC`.

## [v1.4.1] - 2026-06-24

### Bug Fixes

* (debug) Fixed latest-block trace context height handling.
* (rpc) Corrected `eth_feeHistory` output and `effectiveGasPrice` calculation for cached receipts.

## [v1.4.0] - 2026-06-02

### Improvements

* (app) Added a tuned CometBFT HTTP transport so high parallel block fetch counts can keep enough idle connections warm.
* (deps) Updated `sdk-go` and related dependencies.

### Bug Fixes

* (rpc) Allowed an empty `coins_spent` amount when it represents a normalized zero value.

## [v1.3.1] - 2026-05-06

### Bug Fixes

* (indexer) Allowed virtualized Cosmos events to sync before EVM genesis.

## [v1.3.0] - 2026-04-23

### Features

* (rpc) Added native Cosmos x/bank event virtualization as JSON-RPC transactions and logs.
* (jsonrpc) Added robust WebSocket subscriptions backed by indexed data, including virtualized logs.

### Improvements

* (indexer) Migrated the internal KV codec to Cap'n Proto and added `sonic` JSON handling for RPC payloads.
* (tools) Added `kvinspect` for inspecting indexed gateway state.

### Bug Fixes

* (jsonrpc) Preserved connection IDs for JSON-RPC clients.
* (indexer) Fixed virtual transaction Merkle root derivation when virtual transactions are present.
* (indexer) Corrected `TxResult.EthTxIndex` handling for indexed transactions.

## [v1.2.0] - 2026-04-18

### Features

* (indexer) Added parallel forward-tip syncing while historical gaps are still being filled.

### Bug Fixes

* (rpc) Made `eth_blockNumber` follow the latest indexed block.

## [v1.1.1] - 2026-04-13

### Improvements

* (deps) Bumped the Go toolchain version and pinned `sdk-go`.

### Bug Fixes

* (config) Properly distinguished Cosmos chain ID from EVM chain ID.

## [v1.1.0] - 2026-04-09

### Features

* (rpc) Added cache-backed `eth_getBlockReceipts` support and expanded offline RPC-only serving for indexed blocks, headers, transactions, receipts, and logs.
* (telemetry) Added OpenTelemetry tracing through `gotracer` across the app lifecycle, JSON-RPC surfaces, backend cache paths, and the indexer.
* (indexer) Added `resync` CLI command that allows to re-fetch a range of blocks and re-index the data, allows to patch incorrectly indexed state.

### Improvements

* (indexer) Extended the KV cache with richer block metadata, block-hash lookups, grouped block logs, RPC transaction payloads, and trace result storage to reduce dependence on live queries.
* (jsonrpc) Made CometBFT event streams optional so HTTP and WS startup can continue in polling-only mode when subscriptions are unavailable.
* (testing) Expanded parity and benchmark coverage around cache-only/offline operation, `eth_getBlockReceipts`, and trace-heavy historical workloads.
* (logging) Reduced amount of noisy logs and adjusted the severity level INFO/WARN/ERROR.
* (indexer) Changed the validation logic for the "earliest available block", to address issue with sharded archival infrastructure.
* (cli) Refactored the gateway entrypoint onto `mow.cli`, added `start`, `version`, and `resync` commands, and exposed sync/offline runtime flags through the CLI surface.

### Bug Fixes

* (indexer) Fixed EVM tx index normalization, block parsing, and block-result handling for failed or log-less transactions to avoid stale or inconsistent cached state.
* (rpc) Removed extra live calls during block parsing and hardened cached block reconstruction and fallback behavior when indexed data is partial.
* (telemetry) Fixed OTEL collector/TLS wiring and removed the unused StatsD runtime surface from the configuration.

## [v1.0.0] - 2026-04-01

* Initial version release, full index of EVM range since genesis supported.
