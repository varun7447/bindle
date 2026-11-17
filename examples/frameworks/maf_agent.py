"""Microsoft Agent Framework agent traced by Bindle. The model is a stub, so it runs offline and
costs nothing: one tool call with an email address (redacted by Bindle), then
an answer.

    pip install -e sdks/python agent-framework
    set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY, BINDLE_ENDPOINT
    python examples/frameworks/maf_agent.py

To include prompts and responses: export ENABLE_SENSITIVE_DATA=true
"""
import bindle

bindle.init(agent="maf-demo", version="v1", environment="dev")  # before building the agent

import asyncio


from agent_framework import Agent, BaseChatClient, ChatResponse, Content, Message, tool
from agent_framework._middleware import ChatMiddlewareLayer
from agent_framework._tools import FunctionInvocationLayer
from agent_framework.observability import ChatTelemetryLayer


@tool
def lookup_order(email: str) -> str:
    """Find the latest order for a customer."""
    return "order 1234: refund issued"


class RawStub(BaseChatClient):
    OTEL_PROVIDER_NAME = "stub"

    async def _inner_get_response(self, *, messages, stream, options, **kwargs):
        called = any(c.type == "function_result" for m in messages for c in m.contents)
        if not called:
            content = Content.from_function_call(call_id="c1", name="lookup_order", arguments={"email": "bob@example.com"})
        else:
            content = Content.from_text("Your refund is on its way.")
        return ChatResponse(messages=[Message(role="assistant", contents=[content])], response_id="r1", model="stub-model", usage_details={"input_token_count": 40, "output_token_count": 12})


# Same layer order as the framework's own clients (e.g. OpenAIChatClient).
class StubClient(FunctionInvocationLayer, ChatMiddlewareLayer, ChatTelemetryLayer, RawStub):
    pass


async def main():
    agent = Agent(client=StubClient(), name="refund_agent", instructions="Help with refunds.", tools=[lookup_order])
    print(await agent.run("Where is my refund? I'm bob@example.com"))


asyncio.run(main())
bindle.flush()
