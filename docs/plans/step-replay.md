# Plan: step replay

Status: proposal, not started.

## Problem

When a run goes wrong, engineers want to change one thing (the prompt, the model, a tool result) and see what the agent would have done next, without re-running the whole agent against production systems.

## Scope (v1): replay a single LLM step

- From a step in the trace explorer, "Replay" opens the step's captured input (messages, model, parameters) in an editor.
- The user edits it and runs it against a model of their choice, **with their own provider key**, entered for that session in the browser and sent only to Bindle's replay endpoint (not stored).
- The result is shown side by side with the original output, plus tokens, latency and cost. Replays are saved as separate traces tagged `bindle.replay.of=<trace>/<step>`, so they never mix with production data.

Only possible when content capture was on for that step and the step's input is complete (not truncated or offloaded beyond the 1 MiB limit). The button explains why when it's disabled.

## Later (v2): replay from a step onward

Re-running the rest of an agent needs the agent's code, so it has to happen in the customer's environment. The SDK would expose `bindle.replay(trace_id, from_step, overrides)`, which fetches the recorded inputs, feeds recorded tool results back (mocked) unless overridden, and runs the agent locally. This needs a read API scoped to the ingest key's tenant and an explicit opt-in per workspace.

## Design notes

- Redacted values stay redacted in replays: Bindle never had the original, so the editor shows `[REDACTED:EMAIL]` and the user can type a test value.
- Replay calls use the same egress allowlist and SSRF-safe client as the gateway and evals.
- Rate-limited per tenant; the replay endpoint uses console session auth, like the rest of `/api/*`.

## Estimate

About 2 weeks for v1. v2 needs its own design once v1 is in use.
