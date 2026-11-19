# Tracing across agents, queues, people and MCP servers

One agent's work often doesn't fit in one process or one trace. It waits in a queue, pauses for a human's approval, hands over to another agent, or calls tools on an MCP server. Bindle connects these pieces with OpenTelemetry trace context and span links, so each run page shows its **Linked runs** (continued from, continued in, handed off from or to, called by) and the run summary says when a run continues another.

| Boundary | Use | Result in Bindle |
|---|---|---|
| Queue, job table, cron, human approval | `bindle.context_token()` then `bindle.resume(token)` | A new run, linked to the original ("Continued from" / "Continued in") |
| Another agent takes over | `bindle.resume(token, kind="handoff")` | "Handed off from" / "Handed off to" |
| A service that starts its own run per request | `bindle.resume(token, kind="called")` | "Called by" / "Calls" |
| HTTP between agents (A2A, REST) | Standard `traceparent` header | One trace across both services |
| MCP tool calls | `inject_mcp_meta()` / `mcp_server_context()` | The MCP server's spans join the caller's trace |

## Queues, background jobs and human approval

Capture a context token where the work is handed off and store it with the work. It's a short string (about 60 to 200 characters) with the trace ID, the current span, the agent name and the session. It holds no content and no keys.

```python
# Process A: the agent asks for approval and stops.
with bindle.step("request approval", type="decision"):
    db.save_request(order_id, bindle_token=bindle.context_token())

# Process B, hours later: the approver clicked "Approve".
req = db.load_request(order_id)
with bindle.resume(req.bindle_token, name="refund after approval"):
    issue_refund(order_id)          # traced as usual, inside the resumed run
```

```ts
// TypeScript
await bindle.step('request approval', { type: 'decision' }, async () => {
  await db.saveRequest(orderId, { bindleToken: bindle.contextToken() });
});

await bindle.resume(req.bindleToken, { name: 'refund after approval' }, async () => {
  await issueRefund(orderId);
});
```

`resume()` starts a **new run** whose root step links to the step that made the token, and it keeps the original session, so both runs show in the same conversation under Sessions. Waiting hours inside one trace would make that run's duration and waterfall meaningless; separate, linked runs keep both readable. For short hops where one run reads better (a worker that picks a job up within seconds), pass `same_trace=True` (`sameTrace: true`): the work then joins the original run as a child step.

A missing or malformed token never raises: `resume()` starts an unlinked run and logs at debug level.

Sampling is decided per run, so with a sample rate below 1 one of two linked runs can be dropped. The run page then lists the link as "not stored".

## Handoffs to other agents

Pass the token in whatever carries the task to the other agent (a message, a ticket, a function argument), and resume with `kind="handoff"`:

```python
with bindle.resume(task["bindle_token"], name="billing agent", kind="handoff"):
    ...
```

The other agent can be a different service with its own `bindle.init(agent="billing")`. The new run's page says "This run took over from triage", and the triage run lists "Handed off to billing".

## HTTP between agents (A2A and REST)

Agent-to-agent calls over HTTP need nothing Bindle-specific. OpenTelemetry's HTTP instrumentations (for example `opentelemetry-instrumentation-requests`, `-httpx` or `-fastapi` in Python, `@opentelemetry/instrumentation-http` in Node.js) send and read the W3C `traceparent` header, so the callee's spans join the caller's trace. Without an instrumentation, set the header yourself with OpenTelemetry's propagator:

```python
from opentelemetry.propagate import inject
headers = {}
inject(headers)                     # adds traceparent (and tracestate)
httpx.post(url, json=task, headers=headers)
```

If the callee should show as its own run instead, send `bindle.context_token()` in the request body and call `bindle.resume(token, kind="called")` in the handler.

## MCP servers

MCP requests carry OpenTelemetry context in `params._meta`, using the `traceparent` and `tracestate` keys of the OpenTelemetry MCP semantic conventions. On the client, add it to each request's params; in the server, continue it around the handler:

```python
# MCP client
params = bindle.inject_mcp_meta({"name": "lookup_order", "arguments": {"id": "A-17"}})
result = session.send_request("tools/call", params)

# MCP server (any SDK that exposes the request params)
def handle_call_tool(params):
    with bindle.mcp_server_context(params):
        with bindle.step(params["name"], type="tool_call"):
            return run_tool(params)
```

```ts
const params = bindle.injectMcpMeta({ name: 'lookup_order', arguments: { id: 'A-17' } });
// server side:
bindle.withMcpContext(request.params, () => bindle.step(request.params.name, { type: 'tool_call' }, () => runTool(request.params)));
```

`inject_mcp_meta` keeps existing `_meta` keys (such as `progressToken`) and returns the params unchanged outside a run; a missing or invalid `traceparent` on the server side is ignored. If your MCP SDK already propagates trace context itself, you don't need these helpers.

## How it's stored

Links are standard OpenTelemetry span links; `resume()` also sets `bindle.link.kind` (`resumed`, `handoff` or `called`), `bindle.link.trace_id` and `bindle.link.from_agent` on the new run's root step. Any OpenTelemetry SDK can create links Bindle shows; links without a kind are listed as "Linked to" and "Linked from". The API is `GET /api/traces/{id}/links`.

Token format, shared by both SDKs: `b1.<traceparent>[.<base64url JSON {"ts": tracestate, "a": agent, "s": session}>]`.

Example: [`examples/sdk/linked_runs.py`](../../examples/sdk/linked_runs.py).
