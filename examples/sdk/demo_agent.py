"""A richer synthetic agent run for exploring the console locally.

    set -a; . deploy/local/.env; set +a
    python examples/sdk/demo_agent.py            # one run
    python examples/sdk/demo_agent.py --loop     # plus a runaway tool loop (shows an alert)

No real model is called. LLM steps carry made-up token counts for a model
named "example-model", which has no price in the table, so the console shows
the run's cost as partial instead of inventing a number.
"""

import random
import sys
import time

import bindle

bindle.init(agent="refund-agent", version="v3", environment="dev")

MODEL = {"gen_ai.provider.name": "example", "gen_ai.request.model": "example-model"}


def llm(name: str, prompt: str, answer: str, tokens_in: int, tokens_out: int) -> str:
    with bindle.step(name, type="llm_call", **MODEL, **{
        "gen_ai.usage.input_tokens": tokens_in,
        "gen_ai.usage.output_tokens": tokens_out,
        "bindle.input": prompt,
        "bindle.output": answer,
    }):
        time.sleep(random.uniform(0.02, 0.06))
    return answer


@bindle.trace_tool
def get_order(order_id: str) -> dict:
    time.sleep(0.01)
    return {"order_id": order_id, "status": "delivered", "total": 42.5}


@bindle.trace_step(type="retrieval")
def search_policy(query: str) -> list:
    time.sleep(0.015)
    return ["refunds.md#within-30-days", "refunds.md#damaged-items"]


def call_payments(attempt: int) -> None:
    with bindle.step("refund_payment", type="tool_call", **{"bindle.tool.name": "refund_payment", "bindle.attempt": attempt,
                                                            "bindle.input": '{"order_id": "o_981", "amount": 42.5}'}):
        time.sleep(0.02)
        if attempt == 0:
            raise TimeoutError("payments API timed out after 20ms")


def run(loop: bool) -> None:
    with bindle.step("refund-agent run", type="agent"):
        llm("plan", "Customer jane@example.com asks for a refund on order o_981.",
            "Look up the order, check policy, then refund.", 812, 64)
        bindle.decision("route", options=["refund", "escalate"], chosen="refund",
                        rationale="Order is delivered and within the 30-day window.")
        get_order("o_981")
        search_policy("refund window for delivered orders")
        for attempt in range(2):
            try:
                call_payments(attempt)
                break
            except TimeoutError:
                pass
        if loop:
            for _ in range(6):
                get_order("o_981")  # same tool + same input: the loop detector flags this
        with bindle.step("answer_quality", type="eval", **{"bindle.output": '{"score": 0.92}'}):
            time.sleep(0.005)
        llm("respond", "Write the customer reply.", "Your refund of $42.50 is on its way.", 1290, 88)


if __name__ == "__main__":
    run(loop="--loop" in sys.argv)
    bindle.flush()
    print("sent")
