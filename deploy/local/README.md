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

## Troubleshooting: `OutOfMemoryError`

An `OutOfMemoryError` in the logs comes from OpenSearch (the only Java process in the stack). Check which kind:

```bash
docker compose -f deploy/local/compose.yaml logs opensearch | grep -i -A2 OutOfMemoryError
```

- **`Java heap space`**: the heap is too small for the data. The default is 1 GB; raise it with `BINDLE_OPENSEARCH_HEAP=2g make up` (or put it in `deploy/local/.env`).
- **Container exits with code 137 and no Java error**: Docker itself killed it for lack of memory. Give Docker Desktop more memory (Settings → Resources): about 2.5 times the OpenSearch heap, plus about 1 GB for the rest of the stack.
- **`Direct buffer memory`**: very large write requests. Ingest splits writes at 8 MiB, so update to the latest `main` and rebuild (`make up`).

Heap dumps after an out-of-memory error are written to `/tmp` inside the container, not to the data volume.
