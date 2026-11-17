# Plan: hosted LLM gateway

Status: proposal, not started.

## Problem

Some teams can't add an SDK (no code access, many services, other languages), but they can change the base URL their LLM client calls. A gateway that sits between the agent and the model provider gets them traces, cost and loop detection with one config change. It can also do things tracing alone can't: stop a runaway loop or enforce a spend cap before the call is made.

## Scope (v1)

- An HTTPS service, `gateway.bindle.ai`, that accepts the OpenAI Chat Completions and Anthropic Messages request shapes and forwards them to the provider the customer chose. Bedrock follows in v2 (it needs SigV4 with the customer's credentials, which changes the trust model).
- Auth: the customer's Bindle ingest key in one header, their provider key in the provider's usual header. The tenant comes from the Bindle key only.
- **Provider keys are passed through, never stored.** No key vault in v1.
- Each call becomes an `llm_call` step on the same pipeline (Kinesis → processor), with tokens from the provider's response and cost from the same pricing table (unknown model → `cost_known=false`).
- Streaming (SSE) passes through unchanged; the step is written when the stream ends.
- Optional guards, off by default: a per-tenant spend cap per hour, and blocking a call that repeats the same prompt N times in one trace (`x-bindle-trace-id` header).

## Not in v1

Caching, retries/fallbacks across providers, prompt management, stored provider keys.

## Design notes

- Go stdlib reverse proxy (`net/http/httputil`), same deployment shape as ingest: Fargate behind an HTTPS-only ALB.
- Fail open: if Bindle's pipeline is slow or down, the provider call still goes through and the step is dropped, never the request. Latency budget: under 10 ms added at p99, excluding the provider.
- Redaction applies before the step is written, exactly as for ingest. Bodies are never logged.
- Egress only to an allowlist of provider hostnames, so the gateway can't be used as an open proxy.

## Open questions

- Which providers' terms allow a third party to relay requests with the customer's key? Check each provider's terms before launch.
- Pricing for gateway traffic vs. trace ingest.

## Estimate

About 3 weeks for OpenAI + Anthropic shapes, streaming, steps and the two guards, plus load testing.
