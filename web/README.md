# Bindle console

Next.js (App Router) + TypeScript. Pages render on the server and call
`bindle-api` from the Next.js server, so the browser never talks to the API
directly and no credentials reach it.

| Page | Path |
|---|---|
| Overview: KPIs, runs over time, alerts, top failing tools, recent runs | `/` |
| Runs list with filters and pagination | `/runs` |
| Trace explorer: waterfall + step detail (input, output, reasoning, attributes, redaction notice) | `/traces/<trace_id>` |
| Connect an agent: per-framework snippets and a live "waiting for first trace" state | `/connect` |

## Run locally

Start the local stack first (see the repo README), then:

```bash
cd web
npm install
BINDLE_API_URL=http://127.0.0.1:8080 BINDLE_PUBLIC_INGEST_URL=http://localhost:4318 npm run dev
# open http://127.0.0.1:3000
python ../examples/python/demo_agent.py --loop   # sample data, including a runaway-loop alert
```

`npm run dev` and `npm run start` listen on `127.0.0.1:3000` only. The console
has no sign-in yet (step 3) and shows whatever tenant `bindle-api` resolves, so
don't widen the bind.

| Env var | Default | Meaning |
|---|---|---|
| `BINDLE_API_URL` | `http://127.0.0.1:8080` | bindle-api base URL (server-side only) |
| `BINDLE_PUBLIC_INGEST_URL` | `https://ingest.bindle.ai` | Ingest endpoint shown in the Connect snippets |

## Checks

```bash
npm run typecheck && npm test && npm run build
```

## Notes

- Step-type colors come from the product spec. They are not distinguishable
  by color alone (llm vs decision for protanopia; tool vs eval at any vision),
  so every step also shows its type as text.
- Costs come only from known model prices. A run with unpriced model steps
  shows `≥ $x`, or `Unknown` when nothing was priced.
