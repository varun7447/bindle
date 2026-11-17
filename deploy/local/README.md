# Local stack

OpenSearch (traces), PostgreSQL (control plane), a one-off setup job, ingest, the API and the alerts service, in Docker. From the repo root:

```bash
make up        # creates deploy/local/.env with random credentials on first run, then starts everything
make console   # the console at http://localhost:3000
make demo      # sends a couple of demo runs
make down      # stops it; data is kept in the bindle_* volumes
```

Without make:

```bash
./deploy/local/dev-env.sh                              # once: deploy/local/.env (git-ignored)
docker compose -f deploy/local/compose.yaml up -d --build
set -a; . deploy/local/.env; set +a                    # BINDLE_API_KEY, BINDLE_ENDPOINT, passwords
```

| Service | Address |
|---|---|
| Ingest (OTLP/HTTP) | `http://localhost:4318` |
| API | `http://127.0.0.1:8080` |
| OpenSearch | `http://127.0.0.1:9200` (user `admin`, `OPENSEARCH_ADMIN_PASSWORD`) |
| PostgreSQL | `127.0.0.1:5432` (user `bindle_owner`, `POSTGRES_PASSWORD`) |

All ports are published on `127.0.0.1` only. To reach the stack from another machine, use an SSH tunnel (`ssh -L 4318:localhost:4318 host`) rather than widening the publish.

Any OpenTelemetry-instrumented app works without the SDK:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer $BINDLE_API_KEY"
```

If OpenSearch's disk fills past its high watermark, it stops creating indices (`setup` then fails). Free Docker disk space; the block lifts once usage drops.
