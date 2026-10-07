# ADR-0003: The most preferred tier owns any overlap window; dedup by event identity is a safety net

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 6 (planner), 8 (merge)

## Context

Hot and cold tiers both hold the same events for a window of days. A query spanning that window can either read it once or read it from both tiers and deduplicate afterwards.

## Options considered

### Option A: query every tier for its full coverage, dedup after
- Simple planning.
- Works for rows, but aggregate partials cannot be deduplicated: a `count` from each tier just adds up. Measured: 80 brute-force failures instead of 40.

### Option B: disjoint, partition-aligned time slices; hot tier owns the overlap
- Each event is read once, so merged aggregates are exact.
- Split points are aligned up to the cold tier's partition width, so cold reads whole partitions.

## Decision

Option B, with row dedup kept as defence in depth. Because the merge is time-ordered and duplicates share an identical timestamp, the dedup set only needs the IDs seen at the current timestamp: O(1) memory per timestamp. A non-zero duplicate counter in a normal plan signals a bug in slicing.

## Consequences

- **Positive:** exact aggregates across tiers; the cheaper and faster tier answers the overlap.
- **Negative:** correctness depends on accurate coverage; a residual predicate on an aggregate is rejected rather than evaluated after the fact.
- **Follow-ups:** a per-dataset preference policy (cost-optimal vs latency-optimal ownership).

## Evidence

`TestBruteForceCountedOnceAcrossTiers` (40), `TestNaiveModeDoubleCounts` (80 aggregate, 40 rows with 40 dropped), `TestGroupsMergeEqualsSingleSource` (property test over random disjoint splits).
