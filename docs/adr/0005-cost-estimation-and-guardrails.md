# ADR-0005: Price every plan before execution and enforce budgets on actual bytes

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 6 (cost), 7 (executor)

## Context

Scanning archives costs money (object-store query engines bill per TB scanned; some SIEM tiers bill per GB queried). An analyst — or an agent in a loop — can run up a bill without knowing it.

## Options considered

### Option A: run first, report cost after
- No estimation work.
- Surprises are discovered after they are paid for.

### Option B: estimate per slice from engine-native metadata; confirm above a threshold; cap per query and per principal per hour; cancel when actual bytes cross the cap
- Parquet: compressed bytes of only the needed columns over pruned partitions (footer stats). OpenSearch: `_count` with the pushed filter × average document size.

## Decision

Option B. Planning is a separate, free call so the UI shows the price before Run. Agents never get interactive confirmation: above-threshold plans are refused with a message that tells the agent to narrow the query. Actual bytes are charged to a rolling ledger.

## Consequences

- **Positive:** no surprise bills; estimates are conservative (whole partitions) and close (within 1% in the benchmark).
- **Negative:** an extra `_count` round trip for hot slices.
- **Follow-ups:** per-tenant budgets; learning estimate corrections from actuals.

## Evidence

`docs/benchmarks.md` (estimate vs actual), `internal/cost`, budget denial path in `service.Submit`.
