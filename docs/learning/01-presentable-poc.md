# Module 1: Building a presentable POC

A POC impresses when it makes invisible decisions **visible**. Plans, pruning, cost, dedup and provenance are what separate this system from "a search box over some databases." Every presentation choice below serves that one idea.

This module defines the finished demo up front. Every later module then produces a piece of it, not a pile of code you polish at the end.

## The definition of done: a 10-minute demo in five acts

One story runs through everything: the attack chain from the Day 1 data.

| Act | What the audience sees | The architecture point it proves |
|---|---|---|
| **1. The problem** | Catalog explorer: one dataset, two physical homes, two query languages, coverage bars showing the overlap | Data is fragmented by tier and engine; the catalog unifies it |
| **2. Ask once** | Natural-language question → generated IR → plan (tiers chosen, partitions pruned) → **cost estimate before running** → confirm | IR over text-to-SQL; time-tiered routing; cost guardrails |
| **3. Correct answers** | Results stream in, fast tier first; per-source status chips; a **dedup counter** ("40 failures, not 80") | Partial results, overlap ownership, event identity |
| **4. Context** | Enrichment column with an **as-of / current toggle**: ankit's laptop vs meera's | Temporal context; the wrong join blames the wrong person |
| **5. Agents, safely** | Agent builds a cited attack timeline; the planted prompt injection is **flagged and neutralized** | MCP guardrails, provenance, data-is-not-instructions |
| **Close** | One benchmark slide | Numbers instead of adjectives |

Acts 3, 4 and 5 each show a **naive approach failing next to the correct one**. Showing that you know the failure mode is what reads as principal-level.

## Four presentation tracks

### Track A: Architecture docs

- **README**: problem, architecture diagram, demo GIF, quick start, benchmark summary and **honest limitations**. Draw the diagram in Mermaid, which GitHub renders natively, so it stays in sync with the code.
- **Architecture Decision Records** in `docs/adr/`, one per real decision, using `docs/adr/0000-template.md`. Expected ADRs:

| ADR | Decision | Written in module |
|---|---|---|
| 001 | Catalog model and freshness strategy | 2 |
| 002 | Typed IR instead of LLM-generated query text | 3 |
| 003 | Overlap ownership and dedup by event identity | 8 |
| 004 | As-of enrichment instead of current-state joins | 9 |
| 005 | Cost estimation and budget guardrails | 6 |
| 006 | Prompt-injection defense for data flowing to agents | 11 |

An ADR is the strongest artifact you can bring into your first week at DataBahn. It shows how you think, not just what you built.

### Track B: Benchmarks

A `cmd/bench` harness writes `bench/results/*.json`, summarized in `docs/benchmarks.md`.

| Metric | Proves |
|---|---|
| Footer bytes read per Parquet file vs file size | Catalog discovery is cheap |
| Bytes scanned vs total bytes (pruning ratio) | Partition and column pruning work |
| p50/p95 latency per tier | Why streaming partial results matters |
| Estimated cost vs actual bytes scanned (error %) | The cost guardrail is trustworthy |
| Dedup correctness vs `ground_truth.json` | Results are right, not just fast |
| As-of vs current attribution accuracy | Enrichment is right |
| NL → IR accuracy on a golden set of ~20 questions | The NL layer is reliable |

Two rules apply:
- Every number must be reproducible with one command.
- Every result file records the environment it ran in: machine, data volume and commit hash.

### Track C: Scripted demo

`make demo` brings up the stack, refreshes the catalog and runs the five acts, either as a guided terminal script or by driving the UI. Demos fail on stage for boring reasons, so:

- it must be **idempotent**: running it twice works;
- it must be **deterministic**: the NL step has a cached-IR fallback, so a flaky LLM response or a missing network connection can't break Act 2.

### Track D: Demo web UI, an investigation console

Serve it from the Go server as a single page embedded with `go:embed`, so there's no Node build step. Use vanilla JS or htmx.

| Screen | Shows |
|---|---|
| Catalog | Datasets × locations, coverage timeline bars, fields with OCSF paths and capabilities |
| Query | NL box → IR panel → plan panel (tier timeline, partitions pruned, estimated cost) → Run |
| Results | Time-ordered rows, per-source status chips, dedup counter, enrichment column with as-of/current toggle, provenance on hover |
| Investigation | Agent timeline with a citation per step, and a banner when injected content was detected |

Design principle: **show the machinery.** Each panel answers the question an engineer would ask: what ran where, what did it cost, what was skipped, and why should I trust it.

## Revised build order

| Module | Build | Presentable output |
|---|---|---|
| 2 | Catalog | ADR-001, bench: footer bytes |
| 3 | IR | ADR-002, golden NL question set (written now, used in M10) |
| 4–5 | DuckDB and OpenSearch engines | bench: pruning ratio, per-tier latency |
| 6 | Planner + cost | ADR-005, bench: estimate error |
| 7 | Executor (streaming, partial results) | — |
| 8 | Merge + dedup | ADR-003, bench: dedup correctness |
| 9 | Enrichment | ADR-004, bench: attribution accuracy |
| 10 | NL → IR | bench: NL accuracy |
| 11 | MCP agent | ADR-006 |
| 12 | HTTP API + web UI | Acts 1–5 clickable |
| 13 | `make demo`, README polish, benchmark doc, demo GIF | The finished presentation |

## Presentation principles

1. **Lead with the problem, not the tech stack.** Nobody cares that it uses DuckDB until they've felt the pain.
2. **One story, end to end.** The attack chain ties every feature to a consequence.
3. **Show failure next to correctness.** Naive union vs dedup, current vs as-of, obeying vs flagging the injection.
4. **Numbers beat adjectives.** "Read 6 KB of a 340 KB file" beats "efficient".
5. **State limitations honestly.** List what production DataBahn must do that this POC doesn't: scale, multi-tenancy, Iceberg manifests, real auth, SIEM-specific dialects. It signals judgment, not weakness.

## Design questions

1. **Audience.** Who is this demo for: DataBahn engineers in your first week, or leadership? How would you reorder or cut acts for each audience?
2. **Results API.** Plain request/response, Server-Sent Events, or WebSockets for streaming partial results to the UI? Consider what happens when the cold tier takes 40 seconds.
3. **Determinism.** The NL step and the agent depend on an LLM. How do you make Acts 2 and 5 work identically every time without faking them?

## Next actions

1. Answer the five catalog design questions in [Module 2](02-catalog.md), plus the three questions above.
2. After design review, write **ADR-001** from the template before you build the catalog. Writing the decision first is the point.
