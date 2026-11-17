"""Send a few agent steps to a local Bindle stack.

    ./deploy/local/dev-env.sh && set -a && . deploy/local/.env && set +a
    docker compose -f deploy/local/compose.yaml up --build
    pip install -e sdks/python
    python examples/sdk/quickstart.py

Then open the console (console/) or query OpenSearch:
    curl -s -u "admin:$OPENSEARCH_ADMIN_PASSWORD" 'http://127.0.0.1:9200/bindle-steps-*/_search?sort=start_ns:desc&size=5&_source=name,type,status,input' 
"""

import random

import bindle

# api_key and endpoint come from BINDLE_API_KEY / BINDLE_ENDPOINT (deploy/local/.env).
bindle.init(
    agent="support-triage",
    version="v12",
    environment="dev",
)


@bindle.trace_tool
def lookup_customer(email: str) -> dict:
    if random.random() < 0.5:
        raise TimeoutError("crm did not respond")
    return {"customer_id": "c_123", "plan": "pro"}


@bindle.trace_step(type="retrieval")
def search_kb(query: str) -> list:
    return ["refunds-policy.md#2", "faq.md#7"]


def run(message: str) -> str:
    with bindle.step("support-triage run", type="agent"):
        bindle.decision("route", options=["crm", "kb", "human"], chosen="crm",
                        rationale="Refund status needs the customer record.")
        try:
            lookup_customer("jane@example.com")  # redacted before it leaves the process
        except TimeoutError:
            bindle.handoff("human-escalation", reason="crm unavailable")
            return "escalated"
        search_kb("refund timelines")
        return "answered"


if __name__ == "__main__":
    print(run("Where is my refund?"))
    bindle.flush()
