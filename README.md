<img width="959" height="310" alt="Screenshot_20260408_232434" src="https://github.com/user-attachments/assets/744efab9-92a8-4620-9c79-9b26dd22067c" />

## Trace timeouts

`WEB3INJ_JSONRPC_TRACE_TIMEOUT_CAP` controls the maximum requested timeout for
`debug_traceBlockByNumber`, `debug_traceBlockByHash`, `debug_traceTransaction`,
and `debug_traceCall`. It defaults to `30s` and accepts positive Go durations
such as `5s` or `1m`. Invalid, zero, and negative configuration values fail
startup. The equivalent CLI flag is `--rpc-trace-timeout-cap`.

Requests above the configured cap are reduced to that cap; shorter client
timeouts remain unchanged. A custom cap also applies when the client omits
its timeout. The default preserves existing cache keys for omitted timeouts.

The effective timeout is also used for trace caching, so requests such as
`{"tracer":"callTracer","timeout":"10000s"}` share results with `30s` requests
at the default cap and do not reuse previously cached errors for oversized
timeouts.

The cap does not raise the upstream node's own limit. Injective core v1.20.4
only accepts up to `30s`; configure a larger cap only with a backend that
supports it. HTTP and upstream proxy deadlines may end a request earlier.
`WEB3INJ_JSONRPC_EVM_TIMEOUT` is a separate setting for `eth_call`.

## LICENSE

[BUSL](./LICENSE)
(c) 2026 Injective Labs
