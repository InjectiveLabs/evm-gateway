# Gas used and fees in receipts

Status: findings, **no behavior change yet**. This documents how the gateway reports `gasUsed`, `cumulativeGasUsed` and `effectiveGasPrice` today, how that compares to what Injective charges, and the options to make it consistent. It came out of the `ante-failed-txs` migration work (`docs/migrations/ante-failed-txs.md`), which deliberately leaves gas semantics unchanged.

Evidence comes from mainnet (`v1.20.4-bl.2`), the production index snapshot at height `185625758`, and the archival node.

## What Injective charges

The EVM ante handler deducts **`gasLimit × gasFeeCap`** up front and never refunds. Neither unused gas nor the difference between the fee cap and `baseFee + tip` comes back ("we do not refund unused gas", `injective-chain/modules/evm/ante/eth_ante.go`). This holds for successful and failed txs alike:

| Tx | Result | Gas limit | Gas used (chain) | Fee cap | Base fee | Fee charged (`tx` event) |
|---|---|---|---|---|---|---|
| `0x03a649b3…` @185578125 | success | 1,200,000 | 619,633 | 2e8 (tip 0) | 1.6e8 | 2.4e14 = 1,200,000 × 2e8 |
| `0xb605de92…` @185516192 | failed (`code=1`) | 400,000 | 29,927 | 2e8 | | 8.0e13 = 400,000 × 2e8 |

## What the gateway reports

| Receipt field | Successful tx | Failed tx, EVM codespace | Failed tx, no EVM events (message handler errors, panics, block gas limit) |
|---|---|---|---|
| `gasUsed` | execution gas (`txGasUsed` event) | gas from the error log | **gas limit** |
| `effectiveGasPrice` | Ethereum formula `min(feeCap, baseFee + tip)` | same | same |
| `cumulativeGasUsed` | chain gas of preceding Cosmos txs + per-Cosmos-tx running sum of `gasUsed` | same | same, i.e. it includes the **gas limit** |

Consequences:

1. **`gasUsed × effectiveGasPrice` is not the fee paid.**
   - For `0x03a6…`: 619,633 × 1.6e8 ≈ 9.9e13, while 2.4e14 was charged.
   - Failed txs of the last column get closer, because `gasUsed` is the gas limit, but still use `effectiveGasPrice` instead of the fee cap.
   - Explorers and accounting tools that derive fees from receipts under-report fees for every tx.
2. **`cumulativeGasUsed` can decrease within a block.**
   - A failed receipt counts its gas limit, but the next receipt starts again from the chain's real gas used.
   - Ethereum requires `cumulativeGasUsed` to be non-decreasing and to end at the block's `gasUsed`.
   - In the production index, 150 of 312 sampled blocks containing such failures have a decreasing `cumulativeGasUsed`.
3. **Failed and successful receipts measure different things**: a gas limit vs. execution gas.

## Scale

In the production index at `185625758`:
- 790,520 failed txs (in 40,262 blocks) store the gas limit as `gasUsed`.
- For those, the chain-reported gas used is a median of 6% of the gas limit (10th–90th percentile 2%–9%).
- Some panics during execution report 0 gas.

## Related: stale `effectiveGasPrice`

Receipts indexed before `v1.4.1` (`b83e41b`, "corrected effectiveGasPrice calc") keep the old formula's value, e.g. `0x1312d000` instead of `0x9896800` (2× the base fee). That affected about 1.3% of sampled heights. Only a resync of those heights fixes it; no migration covers it yet.

## Options

| Option | `gasUsed` | `cumulativeGasUsed` | Fee from receipt (`gasUsed × effectiveGasPrice`) | Ethereum semantics |
|---|---|---|---|---|
| A. Keep (current) | success: execution gas; failure: gas limit | can decrease | wrong for all txs | violated (monotonicity) |
| B. Chain gas everywhere | execution gas for all | monotonic | wrong for all txs; failed txs now much lower | `gasUsed` as on Ethereum |
| C. Fix cumulative only | unchanged | running sum of receipt `gasUsed` | unchanged | monotonic; ends above block `gasUsed` when failures exist |
| D. Report charged gas | gas limit for all | monotonic | right only if `effectiveGasPrice` = fee cap | differs from Ethereum (`gasUsed` ≠ execution gas) |

Any option that changes stored values needs a resync of affected heights. The candidates are known from the migration scan: every failed tx whose stored `gasUsed` equals its gas limit. B and C touch about 40k heights on mainnet; D touches every height with Ethereum txs.

Before choosing, it's worth confirming what downstream consumers expect: Blockscout, indexers, and the integrator who reported the receipt inconsistencies. Option B matches Ethereum tooling. D matches the money actually spent, but also requires reporting the fee cap as `effectiveGasPrice`.
