# Migration: `ante-failed-txs`

Repairs indexes written by gateway versions up to and including `v1.4.4`, which exposed Ethereum txs whose Cosmos tx failed in the ante handler.

## Background

CometBFT includes a tx in a block before executing it. When an Ethereum tx fails in the EVM ante handler (e.g. `insufficient funds`, `code=5 codespace=sdk`, `gas_used=0`), all ante state is discarded, **including the nonce bump** (the last ante step in injective-core `v1.20.4`). The same signed tx stays valid and can be included again in a later block, where it executes.

The old indexer exposed both inclusions under the same tx hash. Hash-keyed records (tx, receipt, rpc tx) were last-write-wins, so depending on the indexing order:

- `eth_getTransactionReceipt` returned the failed first inclusion (wrong block, `status 0x0`, `gasUsed` = gas limit) while `eth_getTransactionByHash` returned the later one, or vice versa;
- `eth_getBlockReceipts` of either block returned the other block's receipt;
- block traces listed the ante-failed txs as `{"txHash": …, "error": "insufficient balance for transfer"}` entries;
- an in-memory receipt cache kept serving a receipt after the KV record was rewritten.

From this release on, ante-failed txs are not indexed. A failure counts as an ante failure when `code != 0` **and the result has no events**, the codespace is not `evm`, the log has no `failed to execute message` wrapper, and it is not the legacy block-gas-limit case. Events are the decisive signal: for a failed tx, baseapp returns the events of a successful ante handler (the EVM ante always emits the fee event) and none when the ante handler failed. Panics recovered during message execution (e.g. `code=111222`, `code=11 out of gas in location: …`) carry no wrapper but keep the ante events, so they stay visible. Failures after the ante handler consumed the nonce and stay visible with `status 0x0`. Gas reporting is unchanged: such failures still report the gas limit as `gasUsed` (see [`docs/gas-used-semantics.md`](../gas-used-semantics.md)).

The migration fixes **history**. Blocks indexed by the new version are correct without it.

## What the migration does

| Phase | Network | Writes | Description |
|---|---|---|---|
| 1. scan | none | none | Scans local KV: failed txs whose stored `gasUsed` equals their gas limit (what the old indexer stored for every non-EVM failure) and every block listing a tx hash owned by another height. |
| 2. verify | `block_results` of candidate heights only | none | Confirms candidates whose Cosmos tx failed in the ante handler. Post-ante failures match the scan signature too and are rejected here. |
| 3. repair | `block` + `block_results` of affected heights only | affected heights | Resyncs confirmed heights plus every height involved in a hash conflict, then writes a completion marker. |

- Resyncing a height deletes its cached block traces and the per-tx traces of the txs it owns.
- Hash-keyed records owned by another height are never deleted by a reset.
- The completion marker (`KeyPrefixMigration` = `12`, key `ante-failed-txs-v1`) makes a rerun a no-op. `--force` runs it again.
- `--scan-only` and `--dry-run` never write and ignore the marker.
- `--from` / `--to` bound the scan; `--report FILE` writes a JSON report of every phase.

## Requirements

- The new release binary or image.
- **Exclusive access to the data dir.** goleveldb takes a lock, so the service must be stopped, or the migration runs on a copy.
- An **archival** Comet RPC covering every affected height, down to `WEB3INJ_EARLIEST_BLOCK` (`127250000` on mainnet). Verify needs `block_results`; repair needs `block` + `block_results`.
- gRPC endpoint (EVM params at startup), as for `resync`.
- **The same `WEB3INJ_VIRTUALIZE_COSMOS_EVENTS` value as the service.** Repaired heights are rewritten in the configured mode.
- Configuration through exported `WEB3INJ_*` variables, `--env-file FILE` (before the command), or a `.env` file in the working directory. Variables from the env file override the process environment.

## Rollout

### 0. Build the release

```bash
git tag v1.4.6            # next free patch version; v1.4.5 is used by the ibc.evm line
make build                # build/evm-gateway
build/evm-gateway version
make buildx-push TAG=v1.4.6
```

Run the full test suite before tagging: `go test -race ./...`.

### 1. Pre-flight on a snapshot (no downtime)

Copy the data dir of the running service (filesystem / volume snapshot), then on the copy:

```bash
export WEB3INJ_DATA_DIR=/path/to/snapshot
export WEB3INJ_COMET_RPC=http://archival-node:26657
export WEB3INJ_GRPC_ADDR=…
export WEB3INJ_VIRTUALIZE_COSMOS_EVENTS=…   # same as the service

evm-gateway migrate ante-failed-txs --scan-only --report scan.json
evm-gateway migrate ante-failed-txs --dry-run  --report plan.json
jq '{candidates: (.scan.candidates|length), conflicts: (.scan.conflicts|length), heights: (.plan.heights|length)}' plan.json
```

`plan.json` lists every height that will be resynced, with the verdict per tx. The scan reads every indexed tx once; verify fetches `block_results` and repair fetches `block` + `block_results` once per affected height, both with `WEB3INJ_FETCH_JOBS` concurrent fetches (transient fetch errors are retried).

Measured on the mainnet production snapshot at height `185625758` (44 GB, `WEB3INJ_FETCH_JOBS=32`, archival node behind an SSH tunnel):

| Phase | Work | Duration |
|---|---|---|
| scan | 6,052,594 indexed txs, 790,926 candidates in 40,579 heights | 2 min |
| verify | 40,579 candidate heights (`block_results`) | 43 min |
| repair | 24 heights resynced | 3 s |

Verification dominates the runtime: most candidates are post-ante failures (stored `gasUsed` = gas limit), which only `block_results` can tell apart from ante failures. Prefer the snapshot-swap strategy below to keep the downtime to a restart.

### 2. Repair

Pick one strategy.

**A. In place.** Downtime is the migration runtime.

```bash
systemctl stop evm-gateway                 # or: docker compose stop evm-gateway
cp -a $WEB3INJ_DATA_DIR $WEB3INJ_DATA_DIR.bak-v1.4.4
WEB3INJ_COMET_RPC=http://archival-node:26657 \
  evm-gateway migrate ante-failed-txs --report apply.json
# start the new release (step 3)
```

With docker compose (same volume and env as the service):

```bash
docker compose stop evm-gateway
docker compose run --rm -e WEB3INJ_COMET_RPC=http://archival-node:26657 \
  evm-gateway evm-gateway migrate ante-failed-txs --report /apps/data/apply.json
```

**B. Snapshot swap.** Downtime is a restart.

1. Run `migrate ante-failed-txs --report apply.json` with the new binary on the snapshot from step 1.
2. Stop the service, move the migrated snapshot into place as the data dir, and keep the old dir as the backup.
3. Start the new release (step 3). Its gap sync re-indexes the blocks produced since the snapshot with the fixed indexer. The node serving `WEB3INJ_COMET_RPC` must still have those blocks.

### 3. Start the new release

Start the service with the new image and the usual configuration (`WEB3INJ_COMET_RPC` back to the regular node if an archival one was used only for the migration).

### 4. Verify

```bash
G=http://localhost:8545
r(){ curl -s $G -H 'content-type: application/json' -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}"; }
H=0x03a649b3df8386840a76a77d26c156c7675f54ef02de1b786d89539f25eb8f5c

r eth_getTransactionByHash  "[\"$H\"]" | jq -c '.result.blockNumber'               # "0xb0fb28d"
r eth_getTransactionReceipt "[\"$H\"]" | jq -c '.result|{blockNumber,status}'      # 0xb0fb28d, 0x1
r eth_getBlockByNumber '["0xb0fb1f0",false]' | jq -c '.result.transactions|length' # 2 (was 4)
r eth_getBlockReceipts '["0xb0fb28d"]' | jq -c '[.result[].blockNumber]'           # ["0xb0fb28d"]
r eth_getBlockReceipts '["0xb0fb9b1"]' | jq -c '[.result[]|.status]'               # 4 x "0x0" (re-included, failed), then "0x1"
r debug_traceBlockByNumber '["0xb0fb1f0",{"tracer":"callTracer"}]' | jq -c '[.result[]|.result.type]'  # ["CALL","CALL"]
curl -s $G/status/sync | jq .phase
evm-gateway migrate ante-failed-txs   # with the service stopped: "migration already completed"
```

### 5. Rollback

The KV format is unchanged, so the previous release can read a migrated data dir. Its only extra key is the marker, which it ignores. Restore the previous image, optionally with the backup data dir. The previous release would re-introduce the bug for new blocks.

## Production snapshot results

On the production snapshot at height `185625758`:

- **Hidden: 46 Ethereum txs, in 20 heights.** All are `code=5` insufficient-funds ante failures with no events.
- **Duplicate hashes: 9 tx hashes were indexed in two blocks** (8 heights). They are the two reported examples, `185581241`/`185581317`, and `185608930`/`185608942` (the block of the `debug_trace*` report). All are repaired.
- **Resynced: 24 heights.** Gas reporting is unchanged.
- **Kept visible: 4 txs whose message execution panicked** (`code=111222`, and `code=11 out of gas in location: …`). The log-only rule would have hidden them; they consumed their nonce, which is why the rule also requires an empty event list.
- **After the migration:** a rescan finds 0 conflicts, and 46 fewer indexed and candidate txs.
- **Differential check:** 462 sampled heights (repaired, conflict, panic and random heights) were compared with a fresh index built from the archive. `eth_getBlockByNumber` and `eth_getBlockReceipts` are byte-identical except 32 responses that differ only in `effectiveGasPrice`. Those are heights indexed before `v1.4.1` that keep the old formula's value; this is a separate, pre-existing issue described in [`docs/gas-used-semantics.md`](../gas-used-semantics.md).

## Validation performed

The migration was exercised end to end against mainnet data (`v1.20.4-bl.2`):

1. `v1.4.4` indexed `185578125`, `185579953`, then `185577968`, `185579815`, which reproduced the report: receipts of `185578125` were labelled `185577968` with `status 0x0`.
2. `migrate ante-failed-txs` (scan, dry-run, apply) confirmed 10 ante-failed txs (`code=5 sdk`), found 5 hash conflicts, and resynced 4 heights. A rerun was a no-op.
3. The repaired index served byte-identical JSON-RPC responses to a fresh index built by the new release in the opposite block order.

The same legacy state, blocks and block results are embedded as static fixtures in `internal/testutil/mainnetfx` and replayed by the test suite.
