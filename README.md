# FedSearch-lite

A laptop-scale POC of federated search over security telemetry: one question, answered across a hot SIEM tier and a cold S3 archive, enriched with time-aware asset context, without copying data into a central index.

## Day 1: tiered data with a known attack

```
                    cmd/datagen  (deterministic: seed + anchor date)
                         │
     ┌───────────────────┼────────────────────────┐
     ▼                   ▼                        ▼
 out/cold/…parquet   out/hot/…ndjson        out/context/*.csv
 Hive-partitioned    daily-index _bulk      hosts, ip_assignments
     │                   │                        │
     ▼                   ▼                        ▼
   MinIO              OpenSearch               Postgres
 (S3 archive)        (SIEM stand-in)        (Reef stand-in, temporal)
```

### Run it

Prerequisites: Go 1.24+ and Docker with about 3 GB of RAM free.

```bash
make gen      # ~5s → 180 days, ~2.2M events (≈190 MB on disk)
make up       # starts the stack and loads all three stores (first run pulls images)
make verify   # numbers should match out/ground_truth.json
make test
```

Postgres loads its seed only on a fresh volume. If you regenerate the data, run `make reset && make up`.

### Tier layout (defaults)

| Window | Hot (OpenSearch) | Cold (S3 Parquet) |
|---|---|---|
| older than 30 days | – | ✅ |
| 30 → 23 days ago | ✅ | ✅ ← **overlap: every event exists twice** |
| last 23 days | ✅ | – |

You can tune this with `-hot-days` and `-archive-lag-days`. Run `go run ./cmd/datagen -h` for all flags.

### The attack chain (ground truth in `out/ground_truth.json`)

| Stage | When | Tier | What it tests later |
|---|---|---|---|
| Recon: 300 blocked probes from `185.220.101.47` | −45d | cold only | Cold-only routing, partition pruning |
| Brute force: 40 failures + 1 success, `svc_backup` → VPN | −27d | **both** | Dedup by `event_id`. A naive union returns 80 failures instead of 40. |
| Lateral movement: `svc_backup` from `10.20.4.17` → `db-prod-01` | −27d | both | **As-of enrichment.** At that time the IP was `lt-ankit-042`. Today it belongs to `lt-meera-118`. |
| Exfiltration: ~4.2 GB to the attacker from `10.20.6.88` | −10d | hot only | Hot-only routing; ankit's laptop after the DHCP change |

One brute-force event carries a **prompt-injection user agent** (`InjectionUA` in `internal/datagen/events.go`). Day 12 uses it to test whether the agent obeys data that an attacker controls.

### Design decisions worth understanding

- **`event_id` is a content hash stamped at generation.** It plays the role of the pipeline's ingestion ID, and the merge stage needs it to dedup the overlap window. Ask the team on day one how DataBahn stamps and propagates event identity across destinations.
- **Timestamps are truncated to milliseconds.** The Parquet column is `timestamp(millisecond)`, so both tiers hold byte-identical values. Precision mismatch between tiers is a real-world dedup bug.
- **Parquet files are time-sorted with 4096-row row groups and zstd compression.** Sorted files give tight min/max statistics, so engines can skip row groups. Day 4 measures this.
- **Postgres enforces `EXCLUDE USING gist (ip WITH =, validity WITH &&)`.** The database itself guarantees an IP never belongs to two hosts at once. `host_at(ip, ts)` is the correct join, and the `current_ip_owner` view exists to demonstrate the wrong one.
- **Field names are flattened OCSF** (`src_endpoint.ip` → `src_endpoint_ip`). Flat columns keep three very different stores mappable to one catalog.

### Exercises before Day 2

1. Open the MinIO console (http://localhost:9001, `fedsearch` / `fedsearch-secret`) and browse the `dt=` partitions. This layout is what the catalog will discover tomorrow.
2. In OpenSearch, find the brute-force burst with a `terms` aggregation on `user_name` filtered to `status:failure`. Notice that the query language is nothing like SQL. That gap is the compiler's job on Day 5.
3. In psql, compare `SELECT * FROM host_at('10.20.4.17', now() - interval '27 days')` with `current_ip_owner`. Then try to insert an overlapping assignment and watch the constraint reject it.

## Learning modules

This POC is built in guided mode: each module explains the concept, poses design questions, and sets acceptance criteria; you write the code.

- [Module 0: OCSF from first principles](docs/learning/00-ocsf.md)
- [Module 1: Building a presentable POC](docs/learning/01-presentable-poc.md): the five-act demo, ADRs, benchmarks, UI
- [Module 2: The Catalog](docs/learning/02-catalog.md)

## Roadmap

| Day | Package | Concept |
|---|---|---|
| 1 ✅ | `datagen`, `deploy/` | Tiered storage, event identity, temporal context |
| 2 | `catalog` | Schema inference from Parquet footers, partition discovery, sample values |
| 3 | `ir` | Typed query IR plus validation (the contract between the LLM and the engines) |
| 4 | `compiler/duckdb`, `engine/duckdb` | Pushdown, pruning, bytes scanned |
| 5 | `compiler/opensearch`, `engine/opensearch` | Dialect capability gaps |
| 6 | `planner`, `cost` | Time-tiered routing, overlap assignment, pre-execution cost |
| 7 | `exec` | Async fan-out, budgets, cancellation, partial results |
| 8 | `merge` | k-way time merge, decomposable aggregates, dedup |
| 9 | `enrich` | As-of joins |
| 10 | `nl` | Grounded NL → IR |
| 11 | `cmd/mcp` | MCP tools with audit and cost caps; agent loop |
| 12 | hardening | Prompt injection via log data, per-source metrics |
