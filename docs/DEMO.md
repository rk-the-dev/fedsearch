# Demo script (10 minutes)

One story runs through everything: an external IP brute-forces a service account, uses it to reach a production database from a laptop, and exfiltrates 4 GB a few weeks later. The laptop's IP changed hands in between. Each act shows a naive approach failing next to the correct one.

## Before you present

```bash
make gen          # deterministic data: seed 42, anchored to today
make run          # lite mode, no Docker; or `make up && make run-docker`
```

Open http://localhost:8080. Rehearse once with `make demo` (terminal version of the same acts). The console works without an LLM: natural-language questions on the chips come from the golden set. With `ANTHROPIC_API_KEY` set, any question goes to the model and the badge says so.

## Act 1: The problem (Catalog tab, 1.5 min)

- Point at the tier strip for **Authentication**: cold tier (blue) holds April to mid-September, hot tier (amber) holds the last 30 days, and the hatched window is held by both.
- "Two engines, two query languages, and seven days of every event stored twice. Any federated answer has to know that."
- Open **10 OCSF fields and their physical columns**: `status_id` is stored as the caption `status`; the catalog maps between them.
- Optional: **Refresh catalog**. The note shows a few KB of footer read per Parquet file and "all unchanged, served from cache" on the second click.

## Act 2: Ask once (Query tab, 2 min)

- Click the chip **How many failed logins did svc_backup have in the last 6 months?**
- Left: the IR. "The model fills this typed structure; it never writes SQL."
- Right: the plan strip. "Cold answers up to 7 September, hot from 7 September. The overlap goes to one tier, aligned to a partition boundary."
- Open **SQL sent to DuckDB**: the file list is the partition pruning, and `"status" = 'failure'` is the enum transform.
- The price is shown before Run. "Nobody gets a surprise bill, and agents cannot run anything above the threshold."

## Act 3: Correct answers (2 min)

- **Run query**: 40.
- Tick **Naive mode** and run again: 80, with a red note and a hatched overlap on the plan. "This is the bug every naive federation has. Dedup fixes rows; it cannot fix a count. So disjoint slices are a correctness rule, not an optimization."
- Untick naive mode. Click **Show failed logins for svc_backup**: 40 rows, one of them red. "That user agent contains instructions to an AI analyst. You see it; agents never do."

## Act 4: Context (1.5 min)

- Click **Where did svc_backup log in from in the last 6 months?** The top row is svc_backup on `db-prod-01` from 10.20.4.17, highlighted.
- Device column: **lt-ankit-042**. Switch the toggle to **Current owner (naive)**: **lt-meera-118**.
- "Same IP, different laptop. A current-state join sends the incident response to the wrong person."

## Act 5: Agents, safely (Investigate tab, 2 min)

- Indicator `185.220.101.47`, **Investigate**.
- Walk the timeline: reconnaissance (cold tier only), credential access (overlap window), lateral movement (attributed as of event time), collection (750 MB from the database), exfiltration (hot tier only).
- Every entry cites `location/event_id`. Open **Every step, as audited**: five governed queries, their tiers, rows and cost.
- Findings call out the attribution trap and the withheld injection.
- Audit tab: every question, plan, query and tool call, by analyst and agent.

## Close (1 min)

Show `docs/benchmarks.md`:

- under 1% of Parquet bytes read to build the catalog; 0 re-reads when nothing changed
- 81 of 157 partitions and 3 of 11 columns read for an 80-day query
- estimate within about 1% of actual bytes
- 40 not 80; 40 duplicates dropped; as-of attribution correct

Then the honest list: what production needs that this does not do (README, "POC vs production").

## If something breaks on stage

- No LLM or no network: the chips answer from the golden set.
- Docker stack down: `make run` (lite mode) shows the same story from local files.
- Need the terminal: `make demo` runs every act with pauses.
