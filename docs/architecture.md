# Architecture

```
agent + Bindle SDK ──OTLP/HTTP──► bindle-ingest ──► OpenSearch ◄── bindle-api ◄── console
   (redact #1)                    key → tenant        (traces)        │ Cognito sign-in
                                  normalize · redact #2               │
                                  cost · tenant settings              ▼
                                                        PostgreSQL (control plane):
                                                        tenants, users, keys, settings, alerts
```

On AWS, ingest puts batches on Amazon Kinesis and `bindle-processor` writes OpenSearch; locally, ingest writes OpenSearch directly. `bindle-alerts` reads both stores and sends alerts.

## Design rules

1. **No host agent.** Apps send OTLP directly. A collector is optional, for regulated tenants (`deploy/otel-collector`).
2. **Thin SDK, smart server.** Normalization, prices and redaction rules live server-side, so customers never redeploy for them.
3. **Never break the customer's agent.** The SDK exports asynchronously with a bounded queue, swallows its own errors and is a no-op without a key. Ingest answers 429 or 503 with `Retry-After` so exporters back off.
4. **The tenant comes from the API key or session only**, never from the payload or a client parameter.
5. **Redaction twice, on by default.** Secrets are dropped, not masked.
6. **No invented prices.** A model without a known price is reported as unknown cost.

## Ingest and the pipeline

Ingest authenticates the key, decodes OTLP (protobuf or JSON, optionally gzip), normalizes each span into a step, redacts, computes cost and applies the tenant's SDK settings.

- **Locally** (`BINDLE_SINK=opensearch`) it batches and writes OpenSearch itself.
- **On AWS** (`BINDLE_SINK=stream`) it puts each request's steps on a **Kinesis** stream as one gzipped record, keyed by tenant. Anything over a record's 1 MB limit goes to S3 under `spool/`, and the record carries a pointer. If Kinesis throttles, ingest answers 503 so exporters retry.
- **`bindle-processor`** reads the stream and writes OpenSearch. Instances share shards through leases in the control-plane database; after resharding, parent shards are read before their children. Each batch is written before its checkpoint is saved (at-least-once), which is safe because writes are idempotent.

**Large payloads:** an input, output or reasoning field over 32 KB goes to S3 (a local directory in the docker stack) under `tenants/<tenant>/…`, after redaction. The index keeps a 4 KB preview and a reference; the console loads the full text, up to 1 MB per field. A byte budget on ingest's buffer turns floods of large payloads into 503 backpressure rather than memory exhaustion.

## Storage

OpenSearch holds the traces:
- **Steps** go to daily indices (`bindle-steps-YYYY.MM.DD`), deleted after 30 days. Re-sent batches are ignored, so exporter retries never duplicate data. Prompts, outputs and reasoning are full-text indexed.
- **`bindle-traces`** holds one summary per run, recomputed from the steps after each batch.
- **Tenant isolation:** every query goes through one function that adds the tenant filter and routing, so no query can skip it.
- **Least-privilege users:** `bindle_writer` can only add data and `bindle_reader` can only read.

## Accounts and ingest keys

The control plane lives in PostgreSQL, with row-level security per tenant:
- **Tenants, users and memberships.** A user's first sign-in creates their workspace, with them as owner.
- **Ingest keys** look like `bd_live_…` and carry 256 random bits. Only a SHA-256 hash is stored, and the key is shown once. Rotating issues a replacement while the old key keeps working for a grace period (24 hours by default). Revoking takes effect within a minute, the length of ingest's key cache.
- **Least privilege.** Each service has its own database role. Ingest can only resolve a key hash through a database function; the API can't delete anything and sees only its own tenant.

**Console sign-in** uses Amazon Cognito. Without Cognito configured (local development), the API's dev bypass serves the local tenant, and `deploy/local/.env`'s `BINDLE_API_KEY` is seeded as that tenant's key. The bypass is refused on a non-loopback listener.

## Remote SDK settings

Owners and admins can change, from **Settings → SDK settings**, without a redeploy: the sample rate, whether prompts and outputs are captured, and extra redaction patterns. The SDK polls `GET /v1/config` with its ingest key; agents pick changes up within two minutes. These settings are a ceiling: code can only be stricter. Ingest enforces them on every batch too, including from apps the SDK doesn't control. Extra patterns must compile as RE2 without nested repetition, and the built-in redaction can't be turned off.

## Alerts

`bindle-alerts` checks every workspace's rules once a minute. New workspaces start with a runaway-loop rule (the same tool called 5+ times with the same input in one run) and a failure-rate rule (20%+ of runs over an hour, at least 20 runs). An expensive-run rule can be added on the **Alerts** page.

Alerts go to Slack incoming webhooks, HTTPS webhooks or email (Amazon SES) and carry counts, tool names and a console link, never prompt or output content. Each alert fires once: its fingerprint is unique in the control plane.
- **Webhooks are signed:** `X-Bindle-Signature` is `sha256=` plus the HMAC-SHA256 of `X-Bindle-Timestamp + "." + body`, with a per-channel secret shown once.
- **No requests to internal addresses:** customer URLs are checked when saved and again at connect time (private, loopback, link-local, carrier-grade NAT and metadata addresses are refused) and redirects aren't followed.
- **Least privilege:** the alerts role can read rules and channels, add events and record delivery, nothing else.

## Framework support

Spans are normalized from OpenInference, the OpenTelemetry GenAI conventions and Bindle's own attributes. Checked end to end with stub models (see `examples/frameworks/`): LangChain/LangGraph, Strands Agents, Google ADK and Microsoft Agent Framework. Microsoft Foundry Agent Service is supported from its source but not yet run against a real Foundry project.
