# ADR-0002: Every caller produces a typed IR; no engine ever runs text an LLM wrote

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 3 (IR), 10 (NL)

## Context

Questions arrive from analysts, from natural language, and from AI agents. Engines speak different dialects (DuckDB SQL, OpenSearch DSL; in production KQL and SPL too). If an LLM wrote native queries, every dialect would need its own prompt, outputs could not be validated before execution, and a manipulated model could emit arbitrary statements.

## Options considered

### Option A: LLM writes native queries per engine
- Fast to prototype.
- No pre-execution validation, one prompt per dialect, unbounded output.

### Option B: LLM writes SQL; translate SQL to other dialects
- One dialect for the model.
- SQL is too expressive to validate and translate safely; joins and subqueries leak in.

### Option C: LLM fills a small typed IR through a forced tool call; deterministic compilers emit native queries
- The IR is bounded (one dataset, a boolean filter tree, decomposable aggregates, limits).
- Validation lists every problem at once, so the model can repair them in one round.

## Decision

Option C. The IR carries OCSF paths, not physical names; compilers translate through the catalog's bindings, including enum transforms (`status_id = 2` → `status = 'failure'`). Validation enforces limits on span (366 days), rows, groups, list sizes and expression depth. The NL layer gets at most two repairs, then falls back to a golden cache of known-good IR.

## Consequences

- **Positive:** one validation path for every caller; compilers are pure functions with golden tests; demos are deterministic.
- **Negative:** questions the IR cannot express (joins across datasets, sequences) need IR extensions rather than prompt changes.
- **Follow-ups:** sequence/correlation operators (A followed by B within N minutes) as first-class IR, compiled per engine.

## Evidence

`internal/ir` tests; `TestRepairLoop`; `TestGoldenQuestionsPlan` (every golden question plans against the live catalog).
