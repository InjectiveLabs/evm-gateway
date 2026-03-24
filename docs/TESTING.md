# Testing

This repository has two main end-to-end test flows:

- `TestRPCParityAgainstLiveSource`
- `TestSyncOrchestration`

Both are gated behind `WEB3INJ_E2E=1`.

## Local Environment

The current local testing workflow assumes:

- Comet RPC: `http://localhost:26657`
- `injectived` Ethereum JSON-RPC: `http://localhost:8545`
- `injectived` debug RPC: `http://localhost:8545` or `http://localhost:8547`
- gRPC: `127.0.0.1:9900` or `127.0.0.1:9090`

The parity test starts a standalone `web3-gateway` instance on:

- HTTP RPC: `:8646`
- WS: `:8647`

This avoids collisions with the source `injectived` RPC.

## Build

```bash
go build ./cmd/web3-gateway
```

## Run Full E2E Suite

```bash
WEB3INJ_E2E=1 \
WEB3INJ_COMET_RPC=http://localhost:26657 \
WEB3INJ_E2E_SOURCE_RPC=http://localhost:8545 \
WEB3INJ_GRPC_ADDR=127.0.0.1:9900 \
go test -vet=off ./e2e -count=1 -v
```

This runs:

- RPC parity against live `injectived`
- sync orchestration scenarios

## Run Only Parity

```bash
WEB3INJ_E2E=1 \
WEB3INJ_COMET_RPC=http://localhost:26657 \
WEB3INJ_E2E_SOURCE_RPC=http://localhost:8545 \
WEB3INJ_GRPC_ADDR=127.0.0.1:9900 \
go test -vet=off ./e2e -run TestRPCParityAgainstLiveSource -count=1 -v
```

## Run Only Sync Orchestration

```bash
WEB3INJ_E2E=1 \
WEB3INJ_COMET_RPC=http://localhost:26657 \
WEB3INJ_E2E_SOURCE_RPC=http://localhost:8545 \
WEB3INJ_GRPC_ADDR=127.0.0.1:9900 \
go test -vet=off ./e2e -run TestSyncOrchestration -count=1 -v
```

## Parity Test Workflow

`TestRPCParityAgainstLiveSource` does the following:

1. Builds `chain-stresser`.
2. Seeds a small amount of EVM state on the live local chain.
3. Starts a fresh standalone `web3-gateway` with an empty temp DB.
4. Waits until the gateway fully syncs to the source head.
5. Queries both endpoints and compares normalized JSON-RPC responses.
6. Verifies cache-hit counters after synced queries.

The seeding uses the `chain-stresser` deployment accounts file:

- default root: `../chain-stresser`
- accounts file: `chain-stresser-deploy/instances/0/accounts.json`

The parity test automatically runs these seed workloads:

- `tx-eth-send`
- `tx-eth-call`
- `tx-eth-deploy`
- `tx-eth-internal-call`

Each seed workload is intentionally short. The goal is to create stable parity fixtures, not to stress performance.

## Seeding Controls

These env vars tune parity seeding:

- `WEB3INJ_E2E_SEED_DURATION_SEC`
- `WEB3INJ_E2E_SEED_ACCOUNTS_NUM`
- `WEB3INJ_E2E_INTERNAL_CALL_ITERATIONS`
- `WEB3INJ_E2E_SEED_SETTLE_SEC`
- `WEB3INJ_E2E_CHAIN_STRESSER_DIR`

Current defaults are intentionally conservative:

- duration: `2s`
- accounts: `24`
- internal-call iterations: `200`
- settle delay: `2s`

## What Parity Means Here

Strict parity means the test compares normalized JSON-RPC responses from:

- source: local `injectived`
- target: standalone `web3-gateway`

This includes normal requests and batch requests.

Namespaces covered:

- `eth`
- `net`
- `web3`
- `debug`

Notes:

- `web3_clientVersion` is checked semantically, not byte-for-byte. The gateway must identify itself as `web3-gateway/...`.
- `debug` tracing parity is checked against `debug_traceTransaction` with `{"tracer":"callTracer"}` and other chain-facing debug methods.
- process-local runtime debug methods are not meaningful for cross-process parity. Those are smoke-tested on the gateway itself instead.

## Current Parity Coverage

Strict response parity currently covers:

- `net_version`
- `net_listening`
- `net_peerCount`
- `web3_sha3`
- `eth_chainId`
- `eth_syncing`
- `eth_blockNumber`
- `eth_getBlockByNumber`
- `eth_getBlockByHash`
- `eth_getBlockTransactionCountByHash`
- `eth_getBlockTransactionCountByNumber`
- `eth_getLogs`
- `eth_getTransactionByHash`
- `eth_getTransactionReceipt`
- `eth_getTransactionByBlockNumberAndIndex`
- `eth_getTransactionByBlockHashAndIndex`
- `eth_sendRawTransaction`
- `eth_getBalance`
- `eth_getTransactionCount`
- `eth_getStorageAt`
- `eth_getCode`
- `eth_getProof`
- `eth_call`
- `eth_protocolVersion`
- `eth_gasPrice`
- `eth_estimateGas`
- `eth_feeHistory`
- `eth_maxPriorityFeePerGas`
- `eth_getUncleByBlockHashAndIndex`
- `eth_getUncleByBlockNumberAndIndex`
- `eth_getUncleCountByBlockHash`
- `eth_getUncleCountByBlockNumber`
- `eth_hashrate`
- `eth_mining`
- `eth_coinbase`
- `eth_getTransactionLogs`
- `eth_fillTransaction`
- `eth_getPendingTransactions`
- `eth_newFilter`
- `eth_getFilterLogs`
- `eth_getFilterChanges`
- `eth_uninstallFilter`
- `eth_newPendingTransactionFilter`
- `eth_newBlockFilter`
- `debug_traceTransaction`
- `debug_traceBlockByNumber`
- `debug_traceBlockByHash`
- `debug_traceCall`
- `debug_getHeaderRlp`
- `debug_getBlockRlp`
- `debug_printBlock`
- `debug_intermediateRoots`

Batch parity currently covers mixed requests from:

- `eth`
- `net`
- `web3`
- `debug`

See `e2e/rpc_parity_test.go` for the exact request sets.

## Gateway-Only Debug Smoke Coverage

These methods are exercised on the gateway itself, not compared against `injectived` response-for-response:

- `debug_gcStats`
- `debug_memStats`
- `debug_stacks`
- `debug_setGCPercent`
- `debug_setBlockProfileRate`
- `debug_setMutexProfileFraction`
- `debug_freeOSMemory`
- `debug_writeBlockProfile`
- `debug_writeMemProfile`
- `debug_writeMutexProfile`
- `debug_blockProfile`
- `debug_mutexProfile`
- `debug_cPUProfile`
- `debug_goTrace`
- `debug_startCPUProfile`
- `debug_stopCPUProfile`
- `debug_startGoTrace`
- `debug_stopGoTrace`

Important naming detail:

- `CPUProfile` is registered by go-ethereum RPC reflection as `debug_cPUProfile`, not `debug_cpuProfile`.

## Debug RPC Source Selection

The parity test first tries debug methods on `WEB3INJ_E2E_SOURCE_RPC`.

If `debug_traceTransaction` is not available there, it automatically retries on the same host with port `8547`.

This matches the current local `injectived` setup where debug may be exposed on `:8547`.

## Sync Orchestration

`TestSyncOrchestration` validates:

1. sync from beginning
2. sync from middle with restart
3. sync from latest then backfill
4. query while syncing
5. cache-hit vs live-fallback behavior

The test reports sync metrics through `/status/sync`, including cache counters.

## Troubleshooting

- If parity skips immediately, `WEB3INJ_E2E=1` is missing.
- If parity cannot query module data, `WEB3INJ_GRPC_ADDR` is wrong or the node gRPC listener is down.
- If debug parity fails on `:8545`, verify whether `injectived` exposes debug on `:8547`.
- If the chain has no useful EVM fixtures, rerun after seeding or let the parity test seed automatically.
- If you want less seed traffic, reduce the seeding env vars instead of editing the tests.

## File References

- parity test: `e2e/rpc_parity_test.go`
- sync orchestration: `e2e/sync_orchestration_test.go`
