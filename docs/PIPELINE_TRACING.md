# Pipeline Mode 2 tracing

The gateway exposes Chaintable Pipeline's Mode 2 contract as:

```text
trace_debankBlock(blockNumberOrHash)
```

Enable the `trace` JSON-RPC namespace and Injective's EVM gRPC tracer:

```dotenv
WEB3INJ_JSONRPC_API=eth,net,web3,debug,trace
WEB3INJ_VIRTUALIZE_COSMOS_EVENTS=true
```

```toml
# injectived app.toml
enable-grpc-tracing = true
```

The node CLI equivalent is `--evm.enable-grpc-tracing=true`.

Example:

```bash
curl -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"trace_debankBlock","params":["0x1"]}' \
  http://127.0.0.1:8545
```

The response is wire-compatible with Pipeline's `DebankOutPut`: it contains
`block_file`, the Ethereum-compatible `header`, an RLP-encoded `state_diff`,
and `validation_hash`.

The gateway obtains call frames, logs, and exact per-frame storage-write flags
from a single `muxTracer` replay using `erc7562Tracer` and `prestateTracer`.
It completes post-state values with historical `eth_getProof`/`eth_getCode`
queries at the traced height. Native x/bank effects, including MTS tokens and
Circle's Injective USDC implementation, are represented by the gateway's
virtual bank logs at `0x0000000000000000000000000000000000000800`.

Run the focused live compatibility check against an already-running gateway:

```bash
WEB3INJ_E2E_DEBANK=1 \
WEB3INJ_E2E_DEBANK_RPC=http://127.0.0.1:8645 \
WEB3INJ_E2E_DEBANK_BLOCK=0x1 \
go test ./e2e -run '^TestDebankPipelineCompatibilityLive$' -count=1 -v
```
