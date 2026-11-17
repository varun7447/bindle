"""Strands Agents agent traced by Bindle. The model is a stub, so it runs offline and
costs nothing: one tool call with an email address (redacted by Bindle), then
an answer.

    pip install -e sdks/python strands-agents
    set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY, BINDLE_ENDPOINT
    python examples/frameworks/strands_agent.py
"""
import bindle

bindle.init(agent="strands-demo", version="v1", environment="dev")  # before building the agent

from strands import Agent, tool
from strands.models.model import Model


@tool
def lookup_order(email: str) -> str:
    """Find the latest order for a customer."""
    return "order 1234: refund issued"


class StubModel(Model):
    """Asks for the tool once, then answers. Bedrock ConverseStream event shapes."""

    def __init__(self):
        self.turn = 0

    def update_config(self, **kw):
        pass

    def get_config(self):
        return {"model_id": "stub-model"}

    def structured_output(self, *a, **kw):
        raise NotImplementedError

    async def stream(self, messages, tool_specs=None, system_prompt=None, **kwargs):
        self.turn += 1
        yield {"messageStart": {"role": "assistant"}}
        if self.turn == 1:
            yield {"contentBlockStart": {"start": {"toolUse": {"toolUseId": "t1", "name": "lookup_order"}}}}
            yield {"contentBlockDelta": {"delta": {"toolUse": {"input": '{"email": "bob@example.com"}'}}}}
            yield {"contentBlockStop": {}}
            yield {"messageStop": {"stopReason": "tool_use"}}
        else:
            yield {"contentBlockDelta": {"delta": {"text": "Your refund is on its way."}}}
            yield {"contentBlockStop": {}}
            yield {"messageStop": {"stopReason": "end_turn"}}
        yield {"metadata": {"usage": {"inputTokens": 40, "outputTokens": 12, "totalTokens": 52}, "metrics": {"latencyMs": 5}}}


agent = Agent(name="refund_agent", model=StubModel(), tools=[lookup_order], callback_handler=None)
print(agent("Where is my refund? I'm bob@example.com"))
bindle.flush()
