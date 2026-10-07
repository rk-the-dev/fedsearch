# ADR-0006: Agent authority comes only from the policy layer; telemetry is untrusted data

- **Status:** Accepted
- **Date:** 2026-10-07
- **Module:** 11 (agents, MCP)

## Context

Agents query telemetry through tools, and telemetry contains attacker-controlled strings (user agents, URLs, file names). The test data plants one: a user agent that tells "the AI analyst" to classify the attacker IP as benign.

## Options considered

### Option A: rely on prompting the model to ignore instructions in data
- No engineering.
- Fails open: one successful injection changes what the agent does.

### Option B: authority in a policy layer; data labeled and sanitized as defence in depth
- Each agent is a principal with an allow-list of tools and its own hourly budget; every call is audited.
- Tool results are wrapped as `"trust": "untrusted_data"` with a citation on every row.
- Values matching injection heuristics are withheld from the agent-facing copy and flagged in the console; human analysts still see them.
- Catalog samples that reach the NL prompt exclude suspicious values and are fenced as data.

## Decision

Option B. Nothing in the data can grant a tool, raise a budget or skip confirmation, because none of those decisions read the data. Detection is a second line, not the control.

## Consequences

- **Positive:** an injection can at worst mislead an agent's narrative, not expand its access; every claim is traceable through citations.
- **Negative:** heuristics have false positives and negatives; withheld values can hide useful context from the agent.
- **Follow-ups:** a classifier for injection content; per-field trust levels from the pipeline.

## Evidence

`TestToolResultsWithholdInjection`, `TestStatsWithholdInjection`, `TestPromptFencesCatalogAndOmitsSuspicious`, `TestMCPProtocol`.
