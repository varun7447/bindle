# Bindle backend

Go services, one module (`github.com/bindle-ai/bindle/backend`), the standard library plus two direct dependencies: the PostgreSQL driver `lib/pq` and Anthropic's Go SDK (used only by step replay). Dependencies are pinned by `go.mod` and `go.sum` and downloaded at build time (not committed). Go 1.25 or newer; the container image builds all five binaries. On networks that can't reach `proxy.golang.org`, set `GOPROXY=direct`.

```bash
go vet ./... && go test ./...      # from this directory, or `make test-backend` from the repo root
```

Integration tests run when the local stack is up (`make up`) and these are set; each creates and removes its own database, indices and users:

```bash
set -a; . ../deploy/local/.env; set +a
BINDLE_POSTGRES_TEST_URL="postgres://bindle_owner:$POSTGRES_PASSWORD@127.0.0.1:5432/postgres?sslmode=disable" go test ./internal/control/
BINDLE_OPENSEARCH_TEST_URL=http://127.0.0.1:9200 BINDLE_OPENSEARCH_TEST_USER=admin \
  BINDLE_OPENSEARCH_TEST_PASSWORD="$OPENSEARCH_ADMIN_PASSWORD" go test ./internal/search/
```

## Services (`cmd/`)

| Binary | What it does |
|---|---|
| `bindle-ingest` | OTLP/HTTP edge: ingest key → tenant, decode, normalize, redact, cost, apply the tenant's SDK settings, then write OpenSearch (local) or Kinesis (AWS). Also serves `GET /v1/config` to SDKs. |
| `bindle-processor` | AWS only: reads the Kinesis stream and writes OpenSearch (payload offload, run summaries). |
| `bindle-api` | The console's API: runs, traces, steps, overview, insights, alerts, keys, SDK settings. Cognito sign-in. |
| `bindle-alerts` | Checks alert rules every minute and delivers to Slack, signed webhooks or email (SES). No listener. |
| `bindle-migrate` | One-off setup: OpenSearch indices, retention and users; control-plane schema and roles. Idempotent. |

## Packages (`internal/`), by role

| Role | Packages |
|---|---|
| Ingest path | `ingest` (HTTP edge), `otlp` (protobuf/JSON decoding), `normalize` (framework conventions → steps), `redact` (server-side redaction), `cost` (price table), `ratelimit`, `pipeline` (batching), `sink`, `stream` (Kinesis producer/consumer) |
| Storage | `search` (OpenSearch: writer, queries, setup), `payload` (large payloads in S3 or a directory), `query` (what the API reads), `model` (internal data types) |
| Control plane | `control` (PostgreSQL: tenants, users, keys, SDK settings, alert rules, stream leases; `migrations/`), `auth` (ingest key → tenant), `session` (console sign-in), `sdkconfig` (remote SDK settings) |
| API and alerts | `api` (HTTP handlers), `alerts` (rule evaluation and delivery) |
| AWS | `awsv4` (SigV4 signing and credentials, standard library only) |

`pricing.example.json` is the price-table format; prices are never invented, so unknown models are reported as unknown cost.
