# Bindle

Observability for AI agents: every step your agent takes, bundled into one trace.

This repo holds the first end-to-end slice:

```
agent + bindle-sdk ──OTLP/HTTP──► bindle-ingest (Go) ──► ClickHouse
   (redact #1)                   auth · decode · normalize
                                 redact #2 · cost · batch
```

| Path | What it is |
|---|---|
| `sdk/python` | `bindle-sdk`: one-line `bindle.init()`, auto-instrumentation, client-side redaction, `trace_step` / `trace_tool` / `decision` / `handoff` helpers |
| `services` | `bindle-api`: read API for the console (runs, trace waterfall, step detail, overview, alerts) over ClickHouse. `bindle-ingest`: OTLP/HTTP receiver (protobuf + JSON, gzip), API-key → tenant, framework normalizer, server-side redaction, cost, per-tenant rate limit, backpressure, ClickHouse writer. Go standard library only, no third-party dependencies. |
| `schema/clickhouse` | `steps` table, per-trace rollup, reference queries (runs list, waterfall, KPIs, loop detector) |
| `deploy` | `docker-compose.yml` for a local stack |
| `infra/cdk` | AWS CDK (TypeScript): VPC, Fargate ingest behind an HTTPS ALB, secrets, S3 payload bucket |
| `examples/python` | Quickstart agent that sends steps to the local stack |

## Run it locally

```bash
./deploy/dev-env.sh                      # once: random local credentials in deploy/.env (git-ignored)
set -a; . deploy/.env; set +a            # BINDLE_API_KEY, BINDLE_ENDPOINT, CLICKHOUSE_PASSWORD
docker compose -f deploy/docker-compose.yml up --build
pip install -e sdk/python
python examples/python/quickstart.py
curl -u "bindle:$CLICKHOUSE_PASSWORD" http://localhost:8123/ \
  --data "SELECT name, type, status, input FROM bindle.steps ORDER BY start_time FORMAT PrettyCompact"
```

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

Services need Go 1.23 or newer (`go.mod` sets the minimum). The container image builds with Go 1.27.

```bash
cd services && go vet ./... && go test ./...
# optional: query integration test against the local stack
BINDLE_CLICKHOUSE_TEST_URL=http://localhost:8123 CLICKHOUSE_USER=bindle go test ./internal/query/
cd sdk/python && pip install -e '.[dev]' && pytest
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

Verified here: Go vet and tests (decoder, normalizer, redaction, cost, auth, rate limit, backpressure, ClickHouse writer), a local end-to-end run of the ingest binary, and Python SDK unit tests. **Not yet verified:** the SDK's export path against a real `opentelemetry-sdk` install, the ClickHouse schema against a live server, and `cdk synth`. Run all three first.

Next:
- [x] Query API (`bindle-api`)
- [ ] Console (Next.js)
- [ ] Payload offload to S3 for large prompts and outputs
- [ ] Split ingest → Kinesis → processor service (the stream is added to CDK with the processor)
- [ ] Control-plane key service (replaces the env-var bootstrap)
- [ ] Contract tests per framework release for the normalizer mappings
- [ ] TypeScript SDK, `bindle init` CLI, hosted LLM gateway
- [ ] Fill `services/pricing.example.json` with real prices from each provider
