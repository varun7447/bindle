"""Google ADK agent traced by Bindle. The model is a stub, so it runs offline and
costs nothing: one tool call with an email address (redacted by Bindle), then
an answer.

    pip install -e sdks/python google-adk
    set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY, BINDLE_ENDPOINT
    python examples/frameworks/adk_agent.py

To include prompts and responses on the trace:
    export OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental
    export OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=SPAN_ONLY
"""
import bindle

bindle.init(agent="adk-demo", version="v1", environment="dev")  # before building the agent

import asyncio


from google.adk.agents import LlmAgent
from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_response import LlmResponse
from google.adk.runners import InMemoryRunner
from google.genai import types


def lookup_order(email: str) -> str:
    """Find the latest order for a customer."""
    return "order 1234: refund issued"


class StubLlm(BaseLlm):
    """Asks for the tool once, then answers."""

    async def generate_content_async(self, llm_request, stream=False):
        called = any(p.function_response for c in llm_request.contents for p in (c.parts or []))
        if not called:
            part = types.Part(function_call=types.FunctionCall(name="lookup_order", args={"email": "bob@example.com"}))
        else:
            part = types.Part(text="Your refund is on its way.")
        yield LlmResponse(content=types.Content(role="model", parts=[part]),
                          usage_metadata=types.GenerateContentResponseUsageMetadata(prompt_token_count=40, candidates_token_count=12, total_token_count=52))


async def main():
    agent = LlmAgent(name="refund_agent", model=StubLlm(model="stub-model"), tools=[lookup_order], instruction="Help with refunds.")
    runner = InMemoryRunner(agent=agent, app_name="refunds")
    session = await runner.session_service.create_session(app_name="refunds", user_id="u1")
    msg = types.Content(role="user", parts=[types.Part(text="Where is my refund? I'm bob@example.com")])
    async for ev in runner.run_async(user_id="u1", session_id=session.id, new_message=msg):
        if ev.content and ev.content.parts and ev.content.parts[0].text:
            print(ev.content.parts[0].text)


asyncio.run(main())
bindle.flush()
