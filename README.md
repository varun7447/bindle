# Bindle

Observability for AI agents: every step your agent takes, bundled into one trace.

This repo holds the first end-to-end slice:

```
agent + bindle-sdk ──OTLP/HTTP──► bindle-ingest (Go) ──► OpenSearch ◄── bindle-api (Go) ◄── console (Next.js)
   (redact #1)                   key → tenant · decode           │  Cognito sign-in    │
                                 normalize · redact #2           ▼                     │
                                 cost · batch         ──► PostgreSQL (control plane) ◄─┘
                                                          tenants · users · API keys
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

Large payloads: an input, output or reasoning field over 32 KB goes to S3 (a local directory in docker-compose) under `tenants/<tenant>/…`, after redaction. The index keeps a 4 KB preview and a reference; the console loads the full text, up to 1 MB per field. Fields are capped at 1 MB on ingest, and a byte budget on ingest's buffer turns floods of large payloads into 503 backpressure rather than memory exhaustion.

### Accounts and ingest keys

The control plane lives in PostgreSQL:
- **Tenants, users and memberships.** A user's first sign-in creates their workspace, with them as owner. For now each user belongs to one workspace.
- **Ingest keys.** Keys look like `bd_live_…` and carry 256 random bits. Only a SHA-256 hash is stored, and the key is shown once.
  - **Rotate:** issues a replacement; the old key keeps working for a grace period you choose, 24 hours by default.
  - **Revoke:** takes effect within a minute, the length of ingest's key cache. If the database is briefly unavailable, keys used in the last 10 minutes keep working and unknown keys are rejected.
- **Least privilege.** Ingest can only resolve a key hash through a database function and has no table access. The API can't delete anything, and sees only its own tenant's keys.

**Console sign-in** uses Amazon Cognito: the console sends people to Cognito's hosted pages, and the API verifies Cognito's tokens itself. Without Cognito configured (local development), the API's dev bypass serves the local tenant, and `deploy/.env`'s `BINDLE_API_KEY` is seeded as that tenant's key.

### Remote SDK settings

From the console's **SDK settings** page, owners and admins can change three things, and agents pick them up within two minutes without a redeploy:
- the sample rate;
- whether prompts, outputs and reasoning are captured;
- extra redaction patterns.

The SDK polls `GET /v1/config` on the ingest endpoint with its ingest key. These settings are a ceiling: code can only be stricter. Ingest enforces the same settings on every batch, including from apps whose OpenTelemetry setup the SDK doesn't control. Extra patterns must compile as RE2 and may not nest repetition, so they can't slow an agent down, and the built-in redaction can't be turned off.

## Repository layout

| Path | What it is |
|---|---|
| `sdk/python` | `bindle-sdk`: one-line `bindle.init()`, auto-instrumentation, client-side redaction, `trace_step` / `trace_tool` / `decision` / `handoff` helpers |
| `services` | `bindle-api`: read API for the console (runs, trace waterfall, step detail, overview, alerts) over OpenSearch, plus account and ingest-key management, with Amazon Cognito sign-in. `bindle-migrate`: sets up OpenSearch (indices, retention, users) and the control-plane database (schema, roles). `bindle-ingest`: OTLP/HTTP receiver (protobuf + JSON, gzip), API-key → tenant, framework normalizer, server-side redaction, cost, per-tenant rate limit, backpressure, OpenSearch writer. Go standard library plus one vendored dependency, the PostgreSQL driver `lib/pq` (control plane). |
| `web` | Console (Next.js): overview, runs, trace explorer, connect-an-agent. See `web/README.md` |
| `design/console` | Console design mockups (visual spec for `web/`) |
| `docs/images` | README screenshots |
| `deploy` | `docker-compose.yml` for a local stack (OpenSearch, PostgreSQL, setup, ingest, API) |
| `infra/cdk` | AWS CDK (TypeScript): VPC, Fargate ingest behind an HTTPS ALB, OpenSearch Service domain, RDS PostgreSQL (control plane), Cognito user pool, setup task, secrets, S3 payload bucket |
| `examples/python` | `quickstart.py` (minimal) and `demo_agent.py` (richer run with LLM steps, retries and an optional runaway loop) |

## Run it locally

```bash
./deploy/dev-env.sh                      # once: random local credentials in deploy/.env (git-ignored)
set -a; . deploy/.env; set +a            # BINDLE_API_KEY, BINDLE_ENDPOINT, OPENSEARCH_ADMIN_PASSWORD, POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up --build
pip install -e sdk/python
python examples/python/quickstart.py
curl -s -u "admin:$OPENSEARCH_ADMIN_PASSWORD" \
  'http://127.0.0.1:9200/bindle-steps-*/_search?sort=start_ns:desc&size=5&_source=name,type,status,input'
```

### Storage

OpenSearch holds the traces:
- **Steps** go to daily indices (`bindle-steps-YYYY.MM.DD`), deleted automatically after 30 days. Re-sent batches are ignored, so exporter retries never duplicate data. Prompts, outputs and reasoning are full-text indexed.
- **`bindle-traces`** holds one summary per run, recomputed from the steps after each batch.
- **Tenant isolation:** every query goes through one function that adds the tenant filter, so no query can skip it. OpenSearch enforcing isolation itself (document-level security tied to the user's sign-in token) comes with Cognito in step 3.
- **Least-privilege users:** `bindle_writer` (ingest) can only add data, and `bindle_reader` (API) can only read. Neither can delete indices or change security settings.

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

Services need Go 1.23 or newer (`go.mod` sets the minimum) and use only the standard library. The container image builds with Go 1.27.

```bash
cd services && go vet ./... && go test ./...
# optional: integration tests (each creates and removes its own database/indices and users)
BINDLE_POSTGRES_TEST_URL="postgres://bindle_owner:$POSTGRES_PASSWORD@127.0.0.1:5432/postgres?sslmode=disable" go test ./internal/control/
BINDLE_OPENSEARCH_TEST_URL=http://127.0.0.1:9200 BINDLE_OPENSEARCH_TEST_USER=admin \
  BINDLE_OPENSEARCH_TEST_PASSWORD="$OPENSEARCH_ADMIN_PASSWORD" go test ./internal/search/
cd sdk/python && pip install -e '.[dev]' && pytest
cd web && npm install && npm run typecheck && npm test && npm run build
```

## Deploy to AWS

```bash
cd infra/cdk && npm install
npx cdk deploy -c certificateArn=arn:aws:acm:...   # cert for ingest.bindle.ai
```

Synth fails without a certificate: ingest carries API keys and agent payloads, so the public listener is HTTPS on 443 only. For a throwaway dev stack without a certificate, pass `-c insecureDevCidr=<your-ip>/32` instead; it serves plain HTTP reachable only from that range (/24 or narrower).

The stack creates an Amazon OpenSearch Service domain (OpenSearch 3.7): in subnets with no internet route, HTTPS-only (TLS 1.2 with forward secrecy), encrypted at rest and between nodes, with fine-grained access control. Its master and app-user passwords live in Secrets Manager. Defaults are two `m7g.medium.search` data nodes in two Availability Zones with 20 GiB gp3 each; override with `-c searchInstanceType=... -c searchDataNodes=... -c searchVolumeGiB=...` (`t3.small.search` is fine for a dev stack). These resources cost money once deployed.

After deploy:
1. Run the `MigrateCommand` stack output (an `aws ecs run-task` command). It sets up OpenSearch (indices, retention policy, `bindle_writer` and `bindle_reader` users) and the control-plane database (schema, `bindle_api` and `bindle_ingest` roles).
2. Point `ingest.bindle.ai` at the `IngestEndpoint` output.

The stack also creates an RDS PostgreSQL 17 instance for the control plane (`db.t4g.micro` by default; private, encrypted, TLS required, deletion protection) and a Cognito user pool with a hosted sign-in domain (`-c authDomainPrefix=...`, which must be globally unique) and a console app client whose callback is `-c consoleUrl=...` (default `https://app.bindle.ai`). The API and console aren't deployed by the stack yet; the `CognitoIssuer`, `CognitoDomain` and `ConsoleClientId` outputs configure them.

## Design rules

1. **No host agent.** Apps send directly over OTLP. A collector or sidecar is optional, for regulated tenants only.
2. **Thin SDK, smart server.** Normalization, cost tables and redaction rules live server-side, so customers never redeploy for them.
3. **Never break the customer's agent.** The SDK exports asynchronously with a bounded queue, swallows its own errors and becomes a no-op without a key. The edge returns 429 or 503 with `Retry-After` so OTLP exporters back off.
4. **Tenant comes from the API key only**, never from the payload.

## Status and next steps

Verified locally: Go vet and tests (including the OpenSearch integration test), the Python SDK against a real `opentelemetry-sdk`, the console build and tests, the docker-compose stack end to end, and `cdk synth`. Not yet run on AWS.

Next:
- [x] Query API (`bindle-api`)
- [x] Console (`web/`)
- [ ] Payload offload to S3 for large prompts and outputs
- [ ] Split ingest → Kinesis → processor service (the stream is added to CDK with the processor)
- [ ] Control-plane key service (replaces the env-var bootstrap)
- [ ] Contract tests per framework release for the normalizer mappings
- [ ] TypeScript SDK, `bindle init` CLI, hosted LLM gateway
- [ ] Fill `services/pricing.example.json` with real prices from each provider
