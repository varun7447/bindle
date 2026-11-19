# bindle-sdk

Observability for AI agents. Trace every step your agent takes (LLM calls, tool calls, decisions, retries and handoffs) with one line.

```bash
pip install bindle-sdk            # core
pip install 'bindle-sdk[all]'     # plus auto-instrumentation for common frameworks
```

```python
import bindle

bindle.init(api_key="bd_live_...", agent="support-triage", version="v12")
```

That's it. Installed frameworks are detected and instrumented automatically. Frameworks that already emit OpenTelemetry (Strands, Google ADK, Microsoft Agent Framework) are picked up through the global tracer provider; the console's Connect page shows the switches some of them need to include prompts. Microsoft Foundry Agent Service needs `AIAgentsInstrumentor().instrument()` after `bindle.init()`. Prefer `BINDLE_API_KEY` in the environment over a key in code.

## Set up in one command

```bash
bindle init      # detect the framework, save the key to .env, send a test trace, print the code to add
bindle check     # verify the key and endpoint (sends nothing)
```

`bindle init` asks for the key with hidden input (or takes `--key` / `BINDLE_API_KEY`), writes `.env` readable only by you, and adds `.env` to `.gitignore` in git repositories. It never prints the full key.

For coding agents, `bindle mcp` runs an MCP server on stdio with setup-guide, framework-detection, connection-check and test-trace tools. The key is read from the environment and never returned to the model:

```bash
claude mcp add bindle -- bindle mcp
```

## Custom agent code

```python
@bindle.trace_tool
def lookup_customer(email: str) -> dict: ...

@bindle.trace_step(type="retrieval")
async def search_kb(query: str): ...

bindle.decision("route", options=["crm", "kb", "human"], chosen="crm", rationale="needs account data")
bindle.handoff("human-escalation", reason="eval score below threshold")

with bindle.step("plan", type="decision"):
    ...
```

## Serverless

Call `bindle.flush()` before your handler returns.

## Configuration

Every option can be passed to `init()` or set as an environment variable. No config file needed.

| Env var | Default | Meaning |
|---|---|---|
| `BINDLE_API_KEY` | (none) | Ingest key. Without it the SDK is a no-op. |
| `BINDLE_ENDPOINT` | `https://ingest.bindle.ai` | Ingest URL |
| `BINDLE_AGENT` / `BINDLE_AGENT_VERSION` | (none) | Agent name and version |
| `BINDLE_ENVIRONMENT` | (none) | e.g. `prod`, `staging` |
| `BINDLE_SAMPLE_RATE` | `1.0` | Head sampling, 0–1 |
| `BINDLE_REDACT` | `true` | Redact PII and secrets before export |
| `BINDLE_CAPTURE_CONTENT` | `true` | Send prompts and outputs at all |
| `BINDLE_AUTO_INSTRUMENT` | `true` | Detect and instrument frameworks |
| `BINDLE_REMOTE_CONFIG` | `true` | Fetch sampling, capture and redaction settings from the Bindle console |

## Remote settings

The SDK picks up settings changed in the Bindle console (sample rate, content capture, extra redaction patterns) within two minutes, without a redeploy. It polls `/v1/config` on your ingest endpoint in the background.

The console sets a ceiling and your code can only be stricter: the effective sample rate is the lower of the two, prompts and outputs are sent only if both allow it, and console patterns add to the built-in and local ones. Bindle's ingest enforces the same settings for every client, including apps whose OpenTelemetry setup the SDK doesn't control.

## Scores

Record how well a run went, from inside it or later (CI, or an LLM judge grading an exported dataset):

```python
bindle.score("helpful", 0.8)                                  # inside a run
bindle.score("resolved", passed=True, trace_id=line["trace_id"])  # from elsewhere
```

Scores show per agent version on the Agents page. `score()` returns False instead of raising if it can't record.

## Sessions and user feedback

```python
bindle.set_session("conversation-42")          # every span from here on joins the conversation
trace_id = bindle.current_trace_id()            # hand this to your UI with the answer
bindle.feedback(trace_id=trace_id, thumbs_up=False, comment="wrong order")
```

## Linked runs: queues, approvals, handoffs and MCP

```python
token = bindle.context_token()                  # store with the job, message or approval request
with bindle.resume(token, name="after approval"):   # later, anywhere: a new run linked to the first
    ...
params = bindle.inject_mcp_meta(params)         # MCP client: trace context in params._meta
with bindle.mcp_server_context(params): ...     # MCP server: continue the caller's trace
```

`resume(token, kind="handoff")` marks another agent taking over and `kind="called"` a run that serves another run's request. Guide: [`docs/integrations/multi-agent.md`](../../docs/integrations/multi-agent.md).

## CI: `bindle eval`

```bash
bindle eval --dataset evals/refunds.jsonl --checks evals/bindle-checks.json \
  --agent my_app.agent:answer --judge my_app.judge:grade --max-drop 0.05 --report eval-report.md
```

Runs the agent on a dataset exported from Bindle, applies Bindle's checks (and your own judge, with your own keys), compares with the dataset's original answers, and exits 1 on a drop. Example workflow: `examples/ci/bindle-eval.yml`.

## Agent limits

Limits set in the console (pause, max steps or tokens per run, daily budget) apply where your code calls `bindle.check_limits()`:

```python
while not done:
    bindle.check_limits()   # raises bindle.LimitExceeded when paused or over a limit
    step()
```

`LimitExceeded.reason` is `paused`, `over_budget`, `max_steps_per_run` or `max_tokens_per_run`. Steps are counted per run in-process (every span in the trace); tokens come from LLM spans. `bindle.limits_status()` returns the current limits and the run's usage. Without `bindle.init()` or without limits, `check_limits()` does nothing. This is the only place the SDK raises on purpose.

## Safety guarantees

* Export runs on a background thread with a bounded queue. If Bindle is slow or down, spans are dropped, never blocking your agent.
* Remote settings are fetched on a background thread with backoff; if Bindle is unreachable, the last settings stay in effect.
* Every SDK error is caught and logged. The SDK never raises into your code (except your own exceptions, which pass through unchanged).
* Redaction runs in your process before anything is sent, and again server-side.
