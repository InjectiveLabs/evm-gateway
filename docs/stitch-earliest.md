# Earliest EVM history through Stitch

Set `WEB3INJ_STITCH_BACKEND=true` (or `--stitch-backend`) after deploying
Stitch's earliest discovery support. Both configured Comet and gRPC endpoints
must point to the same Stitch backend configuration. The flag defaults to false
and requires online mode.

With the flag enabled, `earliest` asks Stitch to discover the oldest available
EVM snapshot across its shards. Configured shard boundaries and the gateway's
indexer backfill height are not treated as proof that EVM history exists.
Numeric `0x0` remains an alias for `earliest`, matching the existing parser;
positive heights such as `0x1`, block hashes, latest, and pending keep their
existing meanings.

The gateway resolves the snapshot before cache reads or dependent queries using
`/injective.evm.v1.Query/Params` with gRPC metadata:

```text
stitch: earliest
x-stitch-earliest-capability: state
x-cosmos-block-height: 1
```

The height-one value is a compatibility placeholder, not the discovery bound.
The capability is selected for the requested operation:

| Capability | Gateway operations | Required data |
| --- | --- | --- |
| `state` | Transaction count, code, storage | EVM state at H |
| `block` | Block, header, transaction-by-block, receipts | State, Comet block and results at H |
| `execution` | Balance, call, estimate gas, trace call | State and Comet block at H |
| `proof` | Account/storage proof | State, Comet block, EVM/auth proofs at H |
| `trace` | Trace block | State at H and H−1, Comet block/results at H |
| `storage` | Debug storage range | State and EVM subspace query at H |
| `range` | Log boundary, fee history | State, Comet block and results at H |

The response must contain identical positive decimal `x-stitch-earliest-height`
and `x-cosmos-block-height` metadata, plus `x-stitch-backend`. Missing or
inconsistent metadata fails the request, including when a gateway is enabled
against an older Stitch version. Stitch reports incomplete searches as errors;
the gateway does not substitute newer state or invent an empty result.

All dependent reads use the resolved numeric height and private
`x-stitch-backend` affinity so overlapping configured shards cannot redirect a
validated snapshot to another shard that lacks its history. The affinity is
stored in the individual operation's context and attached only to reads at H
(and H−1 for block traces). Comet HTTP and gRPC share that selection; auxiliary
latest reads are not pinned. The `stitch: earliest` marker itself is sent only
for discovery.

A historical absent account is nonce zero; discovery and transport failures
remain errors. Log requests execute the entire resolved range within the
configured limits, and an upstream failure is an error rather than partial
success. Fee history ending at `earliest` returns at most the first EVM block,
because there are no earlier EVM blocks to include. Cache keys and response
block identities use the resolved height.

## Testing

Run `go test ./...` for the regression suite. To include the real Stitch process
with local historical-node fixtures, build the matching Stitch branch and run:

```sh
STITCH_TEST_BINARY=/absolute/path/to/stitch go test ./internal/evm/rpc/backend -run TestStitchEarliestCrossService -count=1
```

This test does not contact a live chain or require a database. It checks shard
fan-out, the actual oldest state, pinned account queries, valid zero/absent
accounts, and incomplete searches.

Tracked by ID-1574, a sub-issue of ID-1568.
