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

That's it. Installed frameworks are detected and instrumented automatically. Frameworks that already emit OpenTelemetry (such as Strands) are picked up through the global tracer provider.

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

## Safety guarantees

* Export runs on a background thread with a bounded queue. If Bindle is slow or down, spans are dropped, never blocking your agent.
* Every SDK error is caught and logged. The SDK never raises into your code (except your own exceptions, which pass through unchanged).
* Redaction runs in your process before anything is sent, and again server-side.
