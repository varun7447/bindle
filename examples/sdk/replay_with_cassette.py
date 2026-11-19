"""Re-run an agent on a recorded run, with every tool answered from the recording.

1. In the console, open a run (Run details) and choose "Download for replay".
2. Run this script on the file:

    pip install -e sdks/python
    python examples/sdk/replay_with_cassette.py run-<trace id>.cassette.json

The agent below gets the recorded run's input, and its tools return what they
returned in that run, so you can change the prompt, the model or the code and
see the new behavior on a real failure without calling live systems. Put
@bindle.trace_tool above @cassette.tool to also record the replayed calls in a
new Bindle run (with bindle.init() as usual).

Without a file, the script uses a small built-in recording.
"""

import json
import sys

import bindle

SAMPLE = {
    "format": "bindle.cassette", "version": 1, "trace_id": "0af7651916cd43dd8448eb211c80319c",
    "agent": "support-triage", "input": "Where is my order A1? jane@example.com",
    "output": "", "incomplete": [],
    "tool_calls": [
        {"tool": "lookup_customer", "arguments": '{"email": "jane@example.com"}',
         "result": '{"customer_id": "c_123", "plan": "pro"}', "status": "success"},
        {"tool": "lookup_order", "arguments": '{"order_id": "A1"}', "result": "", "status": "failure",
         "error": "orders API timed out after 30s"},
    ],
}

cassette = bindle.Cassette.load(sys.argv[1]) if len(sys.argv) > 1 else bindle.Cassette(SAMPLE)


@cassette.tool
def lookup_customer(email: str) -> dict:
    raise AssertionError("not called: answered from the recording")


@cassette.tool
def lookup_order(order_id: str) -> dict:
    raise AssertionError("not called: answered from the recording")


def agent(question: str) -> str:
    """The agent under test. Edit it and re-run against the same recording."""
    customer = lookup_customer("jane@example.com")
    try:
        order = lookup_order("A1")
    except bindle.RecordedToolError as e:
        # The fix being tried: answer gracefully instead of crashing.
        return f"Sorry, I couldn't reach the order system ({e}). A {customer['plan']} agent will follow up."
    return f"Order A1 is {order['status']}."


if __name__ == "__main__":
    print("input: ", cassette.input)
    print("answer:", agent(cassette.input))
    print("replay:", json.dumps(cassette.report(), indent=2))
