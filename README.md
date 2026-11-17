# Bindle

Observability for AI agents: every step your agent takes, bundled into one trace.

This repo holds the first end-to-end slice:

```
agent + bindle-sdk ──OTLP/HTTP──► bindle-ingest (Go) ──► PostgreSQL ◄── bindle-api (Go) ◄── console (Next.js)
   (redact #1)                   auth · decode · normalize      (RLS per tenant)
                                 redact #2 · cost · batch
```

## Console

What a team sees after connecting an agent. Screenshots are from the console in `web/` running locally against synthetic data from `examples/python/demo_agent.py`.

**Overview**: runs, failure rate, latency, cost and tokens, runs over time, runaway-loop alerts, top failing tools, recent runs.

![Bindle console overview](docs/images/console-overview.png)

**Trace explorer**: every step of one run as a waterfall (LLM calls, tool calls, retrieval, decisions, retries, evals), with input, output and reasoning for the selected step. Sensitive values are redacted before storage, and the console says so.

![Bindle trace explorer](docs/images/console-trace.png)

**Connect an agent**: a snippet per framework and a live "waiting for first trace" check, so setup takes minutes.

![Bindle connect-an-agent page](docs/images/console-connect.png)

The design mockups for these screens are in [`design/console/`](design/console/). They render inside the design canvas, not as standalone pages.

## Repository layout

| Path | What it is |
|---|---|
| `sdk/python` | `bindle-sdk`: one-line `bindle.init()`, auto-instrumentation, client-side redaction, `trace_step` / `trace_tool` / `decision` / `handoff` helpers |
| `services` | `bindle-api`: read API for the console (runs, trace waterfall, step detail, overview, alerts) over PostgreSQL. `bindle-migrate`: creates the database roles and schema. `bindle-ingest`: OTLP/HTTP receiver (protobuf + JSON, gzip), API-key → tenant, framework normalizer, server-side redaction, cost, per-tenant rate limit, backpressure, PostgreSQL writer. Go standard library plus one vendored dependency, the PostgreSQL driver `lib/pq`. |
| `web` | Console (Next.js): overview, runs, trace explorer, connect-an-agent. See `web/README.md` |
| `design/console` | Console design mockups (visual spec for `web/`) |
| `docs/images` | README screenshots |
| `deploy` | `docker-compose.yml` for a local stack (PostgreSQL, migrations, ingest, API) |
| `infra/cdk` | AWS CDK (TypeScript): VPC, Fargate ingest behind an HTTPS ALB, secrets, S3 payload bucket |
| `examples/python` | `quickstart.py` (minimal) and `demo_agent.py` (richer run with LLM steps, retries and an optional runaway loop) |

## Run it locally

```bash
./deploy/dev-env.sh                      # once: random local credentials in deploy/.env (git-ignored)
set -a; . deploy/.env; set +a            # BINDLE_API_KEY, BINDLE_ENDPOINT, POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up --build
pip install -e sdk/python
python examples/python/quickstart.py
docker compose -f deploy/docker-compose.yml exec postgres \
  psql -U bindle_owner -d bindle -c "SELECT name, type, status, input FROM bindle.steps ORDER BY start_ns"
```

### Storage

PostgreSQL holds everything. `bindle.steps` is partitioned by day (30-day retention, enforced hourly by ingest) and `bindle.traces` is a per-trace rollup updated in the same statement that inserts steps. Re-sent batches are ignored, so exporter retries never duplicate data. Tenant isolation is enforced twice: every query filters by tenant, and row-level security limits each transaction to the tenant it sets. The app roles are least-privilege: `bindle_writer` (ingest) can only insert, and `bindle_reader` (API) can only read. Neither can run DDL or delete data.

The query API runs on `http://127.0.0.1:8080`:

```bash
curl 'http://127.0.0.1:8080/api/runs?limit=5'
curl "http://127.0.0.1:8080/api/traces/<trace_id>"
curl 'http://127.0.0.1:8080/api/overview?window=24h'
curl 'http://127.0.0.1:8080/api/alerts?window=1h'
```

Console auth arrives with the control plane. Until then `bindle-api` rejects every request with 401, unless `BINDLE_API_DEV_AUTH_BYPASS_TENANT` is set (deploy/.env does this locally), and it refuses that bypass on a non-loopback listener.

The stack publishes its ports on `127.0.0.1` only. Any OpenTelemetry-instrumented agent also works without the SDK:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer $BINDLE_API_KEY"
```

## Tests

Services need Go 1.23 or newer (`go.mod` sets the minimum). The container image builds with Go 1.27. Dependencies are vendored in `services/vendor`.

```bash
cd services && go vet ./... && go test ./...
# optional: PostgreSQL integration test (creates and drops its own database and roles)
BINDLE_POSTGRES_TEST_URL="postgres://bindle_owner:$POSTGRES_PASSWORD@127.0.0.1:5432/postgres?sslmode=disable" go test ./internal/pg/
cd sdk/python && pip install -e '.[dev]' && pytest
cd web && npm install && npm run typecheck && npm test && npm run build
```

## Deploy to AWS

```bash
cd infra/cdk && npm install
npx cdk deploy -c certificateArn=arn:aws:acm:...   # cert for ingest.bindle.ai
```

Synth fails without a certificate: ingest carries API keys and agent payloads, so the public listener is HTTPS on 443 only. For a throwaway dev stack without a certificate, pass `-c insecureDevCidr=<your-ip>/32` instead; it serves plain HTTP reachable only from that range (/24 or narrower).

After deploy: put the ClickHouse Cloud URL and user in the `bindle/clickhouse` secret, run `schema/clickhouse/001_steps.sql` then `002_traces_v2.sql` against ClickHouse, set `bindle/ingest-api-keys` to `key=tenant` pairs, and point `ingest.bindle.ai` at the `IngestEndpoint` output.

## Design rules

1. **No host agent.** Apps send directly over OTLP. A collector or sidecar is optional, for regulated tenants only.
2. **Thin SDK, smart server.** Normalization, cost tables and redaction rules live server-side, so customers never redeploy for them.
3. **Never break the customer's agent.** The SDK exports asynchronously with a bounded queue, swallows its own errors and becomes a no-op without a key. The edge returns 429 or 503 with `Retry-After` so OTLP exporters back off.
4. **Tenant comes from the API key only**, never from the payload.

## Status and next steps

Verified locally: Go vet and tests (including the PostgreSQL integration test), the Python SDK against a real `opentelemetry-sdk`, the console build and tests, the docker-compose stack end to end, and `cdk synth`. Not yet run on AWS.

Next:
- [x] Query API (`bindle-api`)
- [x] Console (`web/`)
- [ ] Payload offload to S3 for large prompts and outputs
- [ ] Split ingest → Kinesis → processor service (the stream is added to CDK with the processor)
- [ ] Control-plane key service (replaces the env-var bootstrap)
- [ ] Contract tests per framework release for the normalizer mappings
- [ ] TypeScript SDK, `bindle init` CLI, hosted LLM gateway
- [ ] Fill `services/pricing.example.json` with real prices from each provider
