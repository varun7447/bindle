<img src="docs/images/logo.svg" alt="Bindle" height="40">

# Bindle

Observability for AI agents: every step an agent takes (LLM calls, tool calls, retrievals, decisions, handoffs) in one trace, with failures, loops, latency and cost explained in plain language.

```python
import bindle
bindle.init(agent="support-triage")   # reads BINDLE_API_KEY; a no-op without it
```

## The console

Three places to look (**Home**, **Runs**, **Alerts**) and one to configure (**Settings**: connect an agent, ingest keys, SDK settings). `⌘K` jumps to any page, a pasted trace ID, or an agent's runs. Light and dark themes follow your OS, with a toggle in the sidebar.

**Home** answers "is anything wrong?" in one line, then lists what needs attention, most severe first: steps that keep failing (grouped, with the latest error), agents with a high failure rate, runaway loops, and model calls with no known price. Each item links to the exact run or step to open. Key numbers are compared with the previous period, and every metric has a plain-language definition behind its `?`.

![Bindle console: Home (light theme)](docs/images/console-home.png)

**Run details** opens with a summary of what the run did: how many LLM and tool calls, what failed and why, whether a retry worked, possible loops, where the time went, and what it cost. Every line links to its step; the waterfall and the selected step's inputs and outputs are below, with the root-cause step selected by default.

![Bindle console: Run details (dark theme)](docs/images/console-trace.png)

**Step inputs and outputs** show as a conversation (system, user, assistant and tool messages, tool calls with their arguments) for every supported framework, with the raw text one click away. Sensitive values are redacted before storage. When content is missing, the console says why: content capture is off for the workspace, off in the agent's code, or the framework doesn't send it by default (with the exact setting to turn on).

![Bindle console: a step as a conversation](docs/images/console-conversation.png)

**Cost** comes from published list prices only (`backend/pricing.json`: Anthropic and OpenAI, with each entry's source and date, including long-prompt tiers). Models without a known price are shown as unknown, never estimated.

Design rationale, colors and accessibility checks: [`docs/design/console.md`](docs/design/console.md).

## Repository layout

| Path | What it is |
|---|---|
| [`backend/`](backend/) | Go services: `bindle-ingest`, `bindle-api`, `bindle-processor`, `bindle-alerts`, `bindle-migrate` |
| [`console/`](console/) | Next.js console |
| [`sdks/python/`](sdks/python/) | `bindle-sdk` for Python, plus the `bindle init` CLI and `bindle mcp` server |
| [`sdks/typescript/`](sdks/typescript/) | `@bindle-ai/sdk` for Node.js |
| [`deploy/local/`](deploy/local/) | Docker Compose stack for development |
| [`deploy/aws/`](deploy/aws/) | AWS CDK stack |
| [`deploy/otel-collector/`](deploy/otel-collector/) | Optional OpenTelemetry Collector config for regulated tenants |
| [`examples/`](examples/) | `sdk/`: quickstart and demo agent. `frameworks/`: LangGraph, Strands, Google ADK and Microsoft Agent Framework agents with stub models |
| [`docs/`](docs/) | [Architecture](docs/architecture.md), [console design](docs/design/console.md), [plans](docs/plans/) |
| [`.github/workflows/`](.github/workflows/) | CI: every test suite, container builds and `cdk synth` on each pull request |

## Run it locally

Needs Docker, Go 1.23+, Node 20+ and Python 3.9+.

```bash
make up          # local stack: OpenSearch, PostgreSQL, ingest, API, alerts
make console     # console at http://localhost:3000 (signed in as the local dev workspace)
make demo        # send demo runs, including one with a runaway loop
```

Then point any agent at it:

```bash
set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY and BINDLE_ENDPOINT
pip install -e sdks/python
python examples/sdk/quickstart.py
```

`make help` lists every target. Details: [`deploy/local/`](deploy/local/).

## Tests

```bash
make test        # Go (vet + tests), Python SDK, TypeScript SDK, console (typecheck, tests, build)
```

CI runs the same checks, plus the control-plane database tests, both container images and `cdk synth`, on every pull request.

Each part can also be run alone (`make test-backend`, `test-python`, `test-typescript`, `test-console`). Integration tests against the local stack are described in [`backend/`](backend/).

## Deploy

See [`deploy/aws/`](deploy/aws/): ingest, a private API, the console behind HTTPS with Cognito sign-in, and the data stores. Deploying creates paid AWS resources.

## Status

Verified locally: all test suites, the Docker stack end to end, framework agents (LangGraph, Strands, ADK, Agent Framework), and `cdk synth`. Not yet run on AWS.

- [x] Ingest, query API, console, alerts, control plane, payload offload, Kinesis pipeline
- [x] Python and TypeScript SDKs, `bindle init`, MCP server, `llms.txt`
- [x] Framework support checked end to end: LangGraph, Strands, Google ADK, Microsoft Agent Framework (Foundry Agent Service from source)
- [x] Console redesign: Home with what needs attention, run summaries, conversations, light and dark themes
- [x] Published prices for Anthropic and OpenAI models; CI on every pull request
- [ ] Google Gemini prices (needs effective-dated entries for scheduled price changes)
- [ ] Contract tests per framework release for the normalizer mappings
- [ ] Hosted LLM gateway, evals, step replay, SOC 2: scoped plans in [`docs/plans/`](docs/plans/)

## License

Bindle is licensed under the [Apache License, Version 2.0](LICENSE). See [NOTICE](NOTICE) for attributions, including the vendored PostgreSQL driver (MIT). Contributions are accepted under the same license; see [CONTRIBUTING.md](CONTRIBUTING.md).
