# Plan: evals

Status: v1 built (see docs/architecture.md, "Evals and replay"). Model keys are never stored: LLM judging runs in the customer's environment, and replay uses a key for one request.

## Problem

Traces show what an agent did; teams also need to know whether it did it well, and whether a new agent version is better or worse than the last one. Today they export traces and score them elsewhere.

## Scope (v1)

- **Datasets from traces.** In the console, select runs (or a filter such as "failed runs this week") and save them as a dataset: the root input and the final output, with redaction already applied.
- **Scorers**, run server-side against a dataset or on a sample of live traffic:
  - deterministic: exact match, contains, JSON schema valid, regex, tool X called / not called, step count and cost under a limit;
  - LLM-as-judge with a rubric the customer writes, using **the customer's own model credentials** (Bindle doesn't pay for or choose the judge model).
- **Version comparison.** Scores grouped by `bindle.agent.version`, so a release shows "v13 vs v12: pass rate, cost, latency".
- **SDK hook** for scores computed in the customer's own tests: `bindle.score(trace_id, name, value)` sent as a span event, so CI results land next to the traces.

## Not in v1

Human annotation queues, synthetic data generation, scheduled evals with alerts (the alerts service can add a "score dropped" rule later).

## Design notes

- Datasets and scorer definitions are control-plane data (PostgreSQL, tenant RLS like everything else). Scores are analytics data: a `bindle-scores-*` index in OpenSearch keyed by tenant, trace and scorer, written by a new `bindle-evals` worker.
- LLM-judge prompts and outputs are stored like any other content: redacted, subject to the workspace's content-capture setting.
- Judge calls go out with the customer's key through the same SSRF-safe client as alert webhooks, to an allowlist of provider hostnames.

## Estimate

About 4 weeks: datasets and deterministic scorers (2), LLM judge and version comparison (2).
