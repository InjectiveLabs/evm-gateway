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

## [Unreleased]

### Bug Fixes

* (debug) \#26 Block traces return one geth-shaped result per visible tx, in `eth_getBlockByNumber` order with `txHash`, for every tracer. Virtual txs (`WEB3INJ_VIRTUALIZE_COSMOS_EVENTS=true`) got a `{"type": 0}` placeholder since v1.4.3. Indexers that decode geth's `callTracer` schema failed on its integer `type`, and `debug_traceTransaction` of a virtual tx found nothing. Virtual txs now get a result built from the tx and receipt served for them. For `callTracer` that is a top-level `CALL` frame with `from`, `to`, `value`, `gas`, `gasUsed`, `input`, `error` when the receipt failed, and the receipt logs with `withLog`. The struct logger, `prestateTracer` and other tracers get their own shapes (see `docs/tracing.md`). `debug_traceTransaction` returns the same result. Placeholders cached by v1.4.3/v1.4.4 are rebuilt on read, so no resync is needed.
* (debug) \#26 Messages of failed Cosmos txs that cannot be replayed, and visible Ethereum txs missing from a replay, get a failed frame instead of an entry-level `error`. `debug_traceTransaction` returns that frame instead of an error. These cases are deterministic, and indexers that treat an entry-level error as a tracing failure retried the block indefinitely.
* (debug) \#26 Block traces of blocks not indexed yet follow the live `eth_getBlockByNumber` tx list. With virtualization they omitted virtual txs, so entries no longer lined up with the block's txs by position. The view reuses the block results fetched for the replay. A block indexed in the other virtualization mode follows the live list too, like `eth_getBlockByNumber`. Synthetic frames, and failed messages traced on their own, are built from the fetched block and block results instead of resolving their tx by hash. So such a trace makes one `Block` and one `BlockResults` request, with no `TxSearch`, and does not fail when the indexer is caught up.
* (indexer) \#16 Ethereum txs whose Cosmos tx failed in the ante handler (nonce not consumed, e.g. insufficient funds) are no longer exposed. The same signed tx can be included again later, which made `eth_getTransactionReceipt`, `eth_getBlockReceipts` and `eth_getTransactionByHash` disagree on its block.
* (indexer) \#16 Re-indexing a block no longer deletes the hash-keyed records of a tx owned by another height; a stale owner is reassigned to the block being indexed.
* (rpc) \#16 In-memory receipt and block-log caches are invalidated whenever indexed data is rewritten, so re-indexed receipts are never served stale.
* (rpc) \#16 Live (cache-miss) block, receipt and tx paths skip ante-failed Ethereum txs.
* (debug) \#16 `debug_traceTransaction` resolves cached block traces by tx hash; it returned another tx's frame or `{"type":0}` when virtual txs preceded the tx.
* (debug) \#16 Block and tx traces no longer replay ante-failed txs; block traces drop entries of non-visible txs (an indexed block without visible txs has an empty trace, also when its block results were unavailable and every message was replayed). This replaces the unreleased synthetic trace for ante-failed txs (`insufficient balance for transfer`, mainnet 185556799): such a tx is not an Ethereum tx, so `debug_traceTransaction` reports it as not found and block traces omit it.
* (indexer) \#16 Cosmos txs with several `MsgEthereumTx` that fail during execution are indexed. The error log names only the failed message, so the indexer rejected the block (`msg index not found in results`) and the syncer skipped it: `eth_getBlockReceipts` listed the txs from the live path, while `eth_getTransactionByHash` and `eth_getTransactionReceipt` returned `null` (mainnet 185519530). Every message of such a tx consumed its nonce and is now a failed tx; the failed message reports the gas used from the log, the others their gas limit. Previously skipped blocks are gaps and are re-indexed by the startup gap sync.
* (rpc) \#16 When a later message of a multi-message Cosmos tx failed, the parsed result of message 0 took the failed message's hash.
* (rpc) \#16 Log `transactionIndex` counts every Ethereum tx of a failed Cosmos tx that the gateway exposes, decoded from the block txs, instead of inferring the count from the error log. Logs after a failed multi-message tx had a `transactionIndex` lower than their receipt's.
* (indexer) \#16 Block sync stops handling fetched blocks as soon as its context is canceled. A block already fetched could still be indexed and published as a new tip after shutdown began, and a cancellation that closed the block stream was logged as `forward sync stopped` instead of returned.
* (debug) \#16 Traces of failed multi-message Cosmos txs follow what ran on chain. Baseapp runs the messages in order, stops at the one that fails and reverts all of them. `debug_traceTransaction` and block trace entries now report a failed frame for messages that never executed (`not executed: message N of the cosmos transaction failed before it`) or that executed and were reverted (`reverted: message N of the cosmos transaction failed`), instead of a successful frame for a tx whose receipt has status `0x0` (mainnet 185519530, `0x02826641…`). `debug_traceTransaction` replays the failed message after the messages that ran before it; a block trace replays it in sequence when it ran first in its Cosmos tx and otherwise traces it alone, like `debug_traceTransaction` (\#26). No trace replays reverted or skipped messages as predecessors, so they cannot change the state seen by later txs.
* (rpc) \#16 Live block, tx and receipt paths filter ante-failed txs with the block results they already fetched instead of requesting them again. A failed second request exposed the ante-failed txs and the extra request doubled the `BlockResults` load.
* (cli) \#16 `--env-file` is now applied: the file is loaded before options take their defaults from the environment. Previously only `.env` in the working directory was read. An explicit file that does not exist is an error.

### Improvements

* (rpc) \#16 Tx lookups by hash or by block and index no longer fall back to the Comet tx index (`TxSearch`) when the indexer holds every block the node has: `WEB3INJ_EARLIEST_BLOCK` is unset or below the node's earliest block (so the indexer starts at the node's history; `history_covered` in `/status/sync`), initial gap sync is done with no gap left, forward sync runs, and a block was synced within the last 10s. The indexer miss is then authoritative, so unknown hashes (pending txs polled by wallets, hidden ante-failed txs) cost no Comet request beyond the mempool check of `eth_getTransactionByHash`, and no longer log `fallback to tendermint tx indexer!`. While the indexer is behind, has unsynced gaps, runs with sync disabled, or starts above the node's history (`WEB3INJ_EARLIEST_BLOCK` set to a height the node still serves), the fallback works as before. `receipt_by_hash.live_fallbacks` in `/status/sync` now counts only receipts built from live data.

### Features

* (docs) \#16 `docs/gas-used-semantics.md` documents how `gasUsed`, `cumulativeGasUsed` and `effectiveGasPrice` relate to the fees Injective charges; gas reporting is unchanged.
* (cli) \#16 Added `migrate ante-failed-txs` to repair existing indexes: offline scan of local state, verification of candidate heights against block results, and resync of affected heights only. See `docs/migrations/ante-failed-txs.md`.

### Security

* (rpc) \#22 Close a WebSocket connection as soon as any response to it cannot be written. Before, the read loop kept serving requests already buffered on the socket of a peer that could not receive them: each pipelined `eth_subscribe` opened another subscription and every other request was proxied to the JSON-RPC server.
* (rpc) \#21 Failed non-EVM txs no longer shift `transactionIndex` of later EVM logs in the block. A failed tx now counts as a failed Ethereum tx only when it has the `evm` or `undefined` codespace and its log ends with the EVM VM error JSON.

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
