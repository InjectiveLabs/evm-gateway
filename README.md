<img width="959" height="310" alt="Screenshot_20260408_232434" src="https://github.com/user-attachments/assets/744efab9-92a8-4620-9c79-9b26dd22067c" />

## Trace timeouts

`debug_traceBlockByNumber`, `debug_traceBlockByHash`, `debug_traceTransaction`,
and `debug_traceCall` accept a requested `timeout` above `30s`, but cap the
effective tracer timeout at Injective's EVM gRPC maximum of `30s`. Shorter
timeouts remain unchanged. HTTP and upstream proxy deadlines may end a request
earlier.

The effective timeout is also used for trace caching, so requests such as
`{"tracer":"callTracer","timeout":"10000s"}` share results with `30s` requests
and do not reuse previously cached errors for oversized timeouts.

## LICENSE

[BUSL](./LICENSE)
(c) 2026 Injective Labs
