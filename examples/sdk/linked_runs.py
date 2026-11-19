"""Linked runs: a refund that waits for a human's approval, then a handoff
to another agent, and a tool call into an MCP server, all connected.

    set -a; . deploy/local/.env; set +a
    pip install -e sdks/python
    python examples/sdk/linked_runs.py

Open the printed run in the console: "Linked runs" shows where it continued
and who it handed off to. The queue and the MCP server are simulated in
this process; in real code they're separate processes, machines or hours.
"""

import json
import queue

import bindle

bindle.init(agent="support-triage", version="v3", environment="dev")
approvals: "queue.Queue[str]" = queue.Queue()


def mcp_server_handle(request: str) -> dict:
    """Stands in for an MCP server: continues the caller's trace from _meta."""
    params = json.loads(request)
    with bindle.mcp_server_context(params):
        with bindle.step(params["name"], type="tool_call", **{"bindle.tool.name": params["name"]}):
            return {"order": params["arguments"]["id"], "amount": 42.0, "status": "delivered"}


# Run 1: triage looks the order up over MCP and asks a human to approve.
bindle.set_session("conversation-7")
with bindle.step("triage refund request", type="agent"):
    request = json.dumps(bindle.inject_mcp_meta({"name": "lookup_order", "arguments": {"id": "A-17"}}))
    order = mcp_server_handle(request)
    with bindle.step("ask for approval", type="decision"):
        approvals.put(json.dumps({"order": order, "bindle_token": bindle.context_token()}))
    first = bindle.current_trace_id()
bindle.set_session(None)

# Run 2, "later": the approver said yes; the run continues the first one.
job = json.loads(approvals.get())
with bindle.resume(job["bindle_token"], name="refund after approval"):
    with bindle.step("issue refund", type="tool_call", **{"bindle.tool.name": "issue_refund"}):
        pass
    handoff_token = bindle.context_token()
    second = bindle.current_trace_id()

# Run 3: the billing agent takes over (normally another service).
with bindle.resume(handoff_token, name="billing follow-up", kind="handoff"):
    with bindle.step("send receipt", type="tool_call", **{"bindle.tool.name": "send_receipt"}):
        pass

bindle.flush()
print(f"first run:  /traces/{first}\nsecond run: /traces/{second}")
