# ADR-0004: Enrich IPs with context as of the event time, not current state

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 9 (enrichment)

## Context

IP addresses change hands: DHCP leases, VPN pools, autoscaling. Results need host and owner context, and the obvious join — the current owner of the IP — attributes old activity to whoever holds the address today.

## Options considered

### Option A: join on the current owner
- One lookup per IP, trivially cacheable.
- Wrong for any event older than the last reassignment. In the test data it blames `lt-meera-118` for lateral movement done from `lt-ankit-042`.

### Option B: temporal assignments with validity intervals; join as of event time
- The context store holds `[valid_from, valid_to)` per IP→host, with a GiST exclusion constraint so an IP can never belong to two hosts at once.
- One batched query per result batch returns all intervals for its distinct IPs; matching happens in memory.

## Decision

Option B. Both views are returned so the console can show the difference, but `as_of` is the answer. Closed intervals are immutable and cached forever; lists with an open interval expire after five minutes.

## Consequences

- **Positive:** correct attribution; one round trip per batch.
- **Negative:** the context store must keep history, not just current state.
- **Follow-ups:** the same pattern for identity (user → role/manager over time) and for asset criticality changes.

## Evidence

`TestAsOfEnrichment`, `TestPostgresAsOf`, `deploy/postgres/init.sql` (exclusion constraint).
