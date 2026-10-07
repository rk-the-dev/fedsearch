# Module 2: The Catalog

Every other component depends on the catalog:

- the **planner** asks where data lives for a time range,
- the **compilers** ask what each field is called in each source,
- the **cost estimator** asks how big the data is,
- the **NL layer** asks which fields and values exist.

When the catalog is wrong, every layer above it is confidently wrong.

> Data substrate: use the Day 1 generator (`make gen && make up`) as a test fixture. It has known ground truth in `out/ground_truth.json`.

## 2.1 What a catalog holds

| Layer | Question | Example |
|---|---|---|
| **Logical** | What kinds of data exist? | Dataset `authentication` = OCSF class 3002 |
| **Physical** | Where does each copy live? | S3 `telemetry-archive/ocsf/authentication/dt=*`, OpenSearch `ocsf-authentication-*` |
| **Coverage** | Which time ranges does each copy hold, and is it complete? | Cold: Apr 10 → Sep 13. Hot: Sep 7 → now. |
| **Schema + mapping** | Columns and their OCSF paths | `src_endpoint_ip` (string) → `src_endpoint.ip` |
| **Statistics** | Size and value distribution | 628K rows, 63 MB; `status` ∈ {success, failure} |
| **Capabilities** | What can each engine do per field? | OpenSearch: aggregatable but not full-text searchable |

Key insight: **one logical dataset, many physical locations.** If each location is modeled as its own table, the planner can never reason about overlap.

## 2.2 Discovery per source type

### Parquet in S3 (cold)

1. List prefixes with `ListObjectsV2` and `Delimiter=/`. Hive-style `key=value` segments are partition columns.
2. Read footers only. A Parquet file ends with `[footer][4-byte length]["PAR1"]`. Two ranged GETs give you the schema, the row count, and per-row-group min/max/null-count.
3. At 1M files, even footer reads are costly. Table formats (Apache Iceberg, Delta) keep manifests so nobody lists S3. Customer BYO buckets are usually raw prefixes, not Iceberg.

### Raw JSON/CSV in BYO buckets (inference)

- Sample N objects per prefix.
- Widen types along the lattice `null → bool → int → float → string`.
- Record confidence ("inferred from 50 samples" vs "declared by the pipeline").
- Expect drift within a prefix.

### OpenSearch (hot)

- `_cat/indices` gives daily indices, so coverage comes from index names, refined with a min/max aggregation on `time`.
- `_mapping` gives field types.
- `_field_caps` gives *searchable* and *aggregatable* per field. That is the capability layer.

### Postgres (context)

`information_schema.columns`. Route it through the same discovery interface.

## 2.3 Field mapping to OCSF

Each physical field maps to an OCSF path, together with:

- **a transform**: e.g. raw `"failure"` → `status_id = 2`. Enum mapping is a transform, not a rename.
- **a provenance**: *declared*, *inferred* or *AI-suggested*.
- **an unmapped list**: fields with no OCSF home, still queryable by raw name.

## 2.4 Sample values are an attack surface

The NL layer needs sample values, or the LLM guesses `action='deny'` when the data says `'blocked'`. But samples flow into LLM prompts, and the dataset contains a planted prompt-injection user agent. So:

- collect top-k values only for **low-cardinality** fields. High-cardinality fields get a cardinality estimate plus a few **truncated, sanitized** examples.
- **mark samples as data**, so the prompt layer can fence them off from instructions.
- never sample fields flagged as secrets or PII.

## 2.5 Freshness and correctness

The catalog is a snapshot. A partition that lands after refresh is **silently missed**, and in security "no results" reads as "no attack."

- **(a)** Scheduled refresh: accept staleness.
- **(b)** Catalog for planning hints, plus a live partition listing at query time.
- **(c)** Event-driven: S3 notifications → catalog updates.

Mature systems combine (b) and (c).

## Design questions (answer before coding)

1. **Data model.** Sketch Source, Dataset, Location, Field, Mapping, Coverage and Stats. How do you represent the same OCSF class in two places with different coverage?
2. **Coverage truth.** Partition names, footer min/max, or both? When do they disagree? (Hint: an event at 23:59:59.999 IST lands in which UTC partition?)
3. **Footer cost.** With 1M Parquet files, what do you cache, keyed by what, so an unchanged refresh does zero footer reads?
4. **Type drift.** `dst_endpoint_port` is `int32` in March files and `string` in April files. What does the catalog record, and what must a compiler do for a query spanning both months?
5. **Staleness.** Which option from 2.5 will the POC use, and what is the worst-case consequence?

## Build: `internal/catalog`

- One `Discoverer` abstraction, with S3/Parquet and OpenSearch implementations against MinIO and OpenSearch.
- Footer reads go through your own `io.ReaderAt` backed by S3 ranged GETs. `parquet-go`'s `OpenFile` accepts any `ReaderAt` plus a size. Count bytes fetched.
- Persist the catalog as JSON. Add CLI commands `catalog refresh` and `catalog show`.

## Acceptance criteria

- [ ] Both datasets discovered in both locations. Coverage matches `hot_from` and `cold_until` in `ground_truth.json`.
- [ ] The overlap window is reported automatically per dataset (Sep 7 → Sep 13 with default flags).
- [ ] Bytes fetched per Parquet file are a tiny fraction of file size. Print both.
- [ ] Every field shows its OCSF path, its type per location, and searchable/aggregatable flags from `_field_caps`.
- [ ] Top values are shown for `status` and `action`. `http_user_agent` shows only sanitized, truncated examples, and the injection string never appears raw in `catalog show`.
- [ ] A second `refresh` with no data changes performs **zero** footer reads.
- [ ] **Stretch:** upload one extra Parquet file with an added column, and have refresh report the drift.
