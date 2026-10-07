# ADR-0001: Model the catalog as logical datasets with many physical locations, and list partitions live at plan time

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 2 (catalog)

## Context

The same OCSF class lives in several places: a hot SIEM tier with short retention and a cold object-store archive with long retention, and for a window of days in both. The planner must reason about which copy answers which part of a time range, so the catalog has to represent one dataset with several locations, each with its own coverage, field names, capabilities and cost model.

The catalog is also a snapshot. Data keeps arriving. A partition written after the last refresh would be silently skipped if the planner trusted the snapshot, and in security an empty result reads as "no attack".

## Options considered

### Option A: one table per physical location
- Simple; mirrors how engines expose data.
- The planner cannot see that two tables are the same events, so it cannot reason about overlap.

### Option B: logical dataset → locations, catalog as source of truth
- Enables overlap reasoning.
- New partitions are invisible until the next refresh: silent data loss.

### Option C: logical dataset → locations, catalog as a planning hint plus live partition listing
- Same model as B. At plan time the planner lists partitions inside the query range directly from each source (one bounded list call). Stats for partitions the catalog does not know yet are estimated from location averages and flagged `stale_stats`.

## Decision

Option C. The cost is one small list call per slice. The benefit is that freshness can never make an answer wrong; it can only make an estimate rough.

Discovery reads Parquet footers only, through a byte-counting `io.ReaderAt` over ranged GETs, and reuses partitions whose ETag is unchanged.

## Consequences

- **Positive:** overlap is first-class (`Catalog.Overlaps`); late partitions are always read; refresh cost is proportional to change.
- **Negative:** plan latency includes a list call per slice; estimates for brand-new partitions are averages.
- **Follow-ups:** at production scale, read table-format manifests (Iceberg/Delta) where they exist and update the catalog from object-store event notifications instead of polling.

## Evidence

`docs/benchmarks.md`: about 1.3–2.2 KB read per Parquet file (under 1% of bytes); 0 partitions re-read on an unchanged refresh. Tests: `TestRefreshLite`, `TestOpenSearchDiscovery`.
