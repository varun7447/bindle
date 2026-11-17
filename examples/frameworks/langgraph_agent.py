"""LangGraph agent traced by Bindle. The model is a stub, so it runs offline and
costs nothing: one tool call with an email address (redacted by Bindle), then
an answer.

    pip install -e 'sdks/python[langchain]' langgraph
    set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY, BINDLE_ENDPOINT
    python examples/frameworks/langgraph_agent.py

Set LOOPS=6 to make the tool repeat and trigger the runaway-loop alert.
"""
import bindle

bindle.init(agent="langgraph-demo", version="v1", environment="dev")  # before building the agent

from typing import TypedDict

from langchain_core.language_models.fake_chat_models import FakeListChatModel
from langchain_core.tools import tool
from langgraph.graph import END, START, StateGraph

llm = FakeListChatModel(responses=["Plan: look up the order", "Your refund is on its way"])


@tool
def lookup_order(email: str) -> str:
    """Find the latest order for a customer."""
    return "order 1234: refund issued"


class S(TypedDict):
    q: str
    a: str


def plan(s):
    return {"a": llm.invoke(s["q"]).content}


def act(s):
    for _ in range(int(__import__("os").environ.get("LOOPS", "1"))):
        lookup_order.invoke({"email": "bob@example.com"})
    return {}


def answer(s):
    return {"a": llm.invoke(s["a"]).content}


g = StateGraph(S)
for name, fn in [("plan", plan), ("act", act), ("answer", answer)]:
    g.add_node(name, fn)
g.add_edge(START, "plan"); g.add_edge("plan", "act"); g.add_edge("act", "answer"); g.add_edge("answer", END)

print(g.compile().invoke({"q": "Where is my refund? email bob@example.com"}))
bindle.flush()
