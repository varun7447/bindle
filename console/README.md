# Bindle console

Next.js (App Router) + TypeScript. Pages render on the server and call
`bindle-api` from the Next.js server, so the browser never talks to the API
directly and no credentials reach it.

| Page | Path |
|---|---|
| Home: one-line status, what needs attention (grouped failures, struggling agents, loops), KPIs vs the previous period, runs over time, agents | `/` |
| Runs: status buttons, filters, pagination | `/runs` |
| Run details: plain-language summary, where the time went, waterfall, step detail (input, output, reasoning, attributes, redaction notice) | `/traces/<trace_id>` |
| Alerts: rules, channels, test sends, history | `/alerts` |
| Settings → Connect an agent: per-framework snippets and a live "waiting for first trace" state | `/settings/connect` |
| Settings → Ingest keys: create (shown once), rotate with a grace period, revoke | `/settings/keys` |
| Settings → SDK settings: sample rate, content capture, extra redaction patterns | `/settings/sdk` |

`⌘K` / `Ctrl+K` opens a command palette (pages, a pasted trace ID, an agent's runs). Design rationale: [`docs/design/console.md`](../docs/design/console.md).

## Run locally

From the repo root: `make up` (the local stack), then `make console`. Or:

```bash
cd console
npm install
BINDLE_API_URL=http://127.0.0.1:8080 BINDLE_PUBLIC_INGEST_URL=http://localhost:4318 npm run dev
# open http://127.0.0.1:3000
```

`npm run dev` and `npm run start` listen on `127.0.0.1:3000` only.

## Sign-in

With the Cognito variables set, every page requires sign-in through Amazon
Cognito's hosted pages (OAuth 2.0 authorization code flow with PKCE). The
session is an AES-256-GCM-sealed, httpOnly, SameSite=Lax cookie holding only
the Cognito access token; the console forwards it to `bindle-api`, which
verifies it. Key changes are Server Actions, which Next.js and the console
both restrict to this console's own origin. A user's first sign-in creates
their workspace.

Without the Cognito variables (local development), there is no sign-in and
the console shows whatever tenant `bindle-api`'s dev bypass resolves, so
don't widen the bind.

| Env var | Default | Meaning |
|---|---|---|
| `BINDLE_API_URL` | `http://127.0.0.1:8080` | bindle-api base URL (server-side only) |
| `BINDLE_PUBLIC_INGEST_URL` | none (snippets show a placeholder) | Ingest endpoint shown in the Connect snippets |
| `BINDLE_COGNITO_DOMAIN` | (none) | Cognito hosted UI, e.g. `https://<prefix>.auth.<region>.amazoncognito.com` |
| `BINDLE_COGNITO_CLIENT_ID` / `BINDLE_COGNITO_CLIENT_SECRET` | (none) | The console's app client (the secret is optional) |
| `BINDLE_CONSOLE_URL` | (none) | This console's public URL (https), used for the OAuth redirect |
| `BINDLE_SESSION_SECRET` | (none) | 32 random bytes, base64 (`openssl rand -base64 32`) |

## Checks

```bash
npm run typecheck && npm test && npm run build
```

## Notes

- Themes: light and dark tokens in `app/globals.css`, from the validated
  data-viz palette. The OS setting picks the theme; the sidebar toggle saves a
  choice per browser.
- Step-type colors are categorical slots validated in both themes; three
  light-mode slots are below 3:1 contrast, so every step also shows its type
  as text.
- Costs come only from known model prices. A run with unpriced model steps
  shows `≥ $x`, or `Unknown` when nothing was priced.
