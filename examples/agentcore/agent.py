"""A Strands agent on Amazon Bedrock AgentCore Runtime, traced by Bindle.

AgentCore Runtime runs this container and calls POST /invocations on port
8080. Bindle needs only bindle.init() and BINDLE_API_KEY, the same as
anywhere else; see README.md for the two observability setups (Bindle only,
or Bindle and CloudWatch together).

Locally, without AWS (a stub model, no Bedrock calls):

    set -a; . deploy/local/.env; set +a
    BINDLE_EXAMPLE_STUB_MODEL=1 PORT=8090 python examples/agentcore/agent.py
    curl -s localhost:8090/invocations -H 'content-type: application/json' \\
      -d '{"prompt": "Where is my refund? I am bob@example.com"}'
"""

import os

import bindle


def ingest_key():
    """BINDLE_API_KEY, or the secret named by BINDLE_API_KEY_SECRET_ARN.

    On AgentCore, prefer the secret: runtime environment variables are part
    of the runtime's configuration, a Secrets Manager secret isn't. The
    execution role needs secretsmanager:GetSecretValue on that secret.
    """
    if os.environ.get("BINDLE_API_KEY"):
        return None  # bindle.init() reads it
    arn = os.environ.get("BINDLE_API_KEY_SECRET_ARN")
    if not arn:
        return None
    try:
        import boto3

        return boto3.client("secretsmanager").get_secret_value(SecretId=arn)["SecretString"].strip()
    except Exception as exc:  # never stop the agent over observability
        print(f"bindle: could not read the ingest key secret: {exc}")
        return None


# Before building the agent. Reads BINDLE_API_KEY and BINDLE_ENDPOINT; does
# nothing without a key. If AWS's ADOT instrumentation is active, Bindle adds
# itself to that tracer provider instead of replacing it.
bindle.init(ingest_key(), agent="agentcore-refunds", version="v1", environment=os.environ.get("BINDLE_ENVIRONMENT", "dev"))

from bedrock_agentcore.runtime import BedrockAgentCoreApp  # noqa: E402
from strands import Agent, tool  # noqa: E402


@tool
def lookup_order(email: str) -> str:
    """Find the latest order for a customer."""
    return "order 1234: refund issued on Nov 12"


def build_model():
    if os.environ.get("BINDLE_EXAMPLE_STUB_MODEL") != "1":
        return None  # Strands' default model on Amazon Bedrock
    from strands.models.model import Model

    class StubModel(Model):
        """Asks for the tool once, then answers. No network calls."""

        def update_config(self, **kw):
            pass

        def get_config(self):
            return {"model_id": "stub-model"}

        def structured_output(self, *a, **kw):
            raise NotImplementedError

        async def stream(self, messages, tool_specs=None, system_prompt=None, **kwargs):
            used_tool = any("toolResult" in block for m in messages for block in m.get("content", []))
            yield {"messageStart": {"role": "assistant"}}
            if not used_tool:
                yield {"contentBlockStart": {"start": {"toolUse": {"toolUseId": "t1", "name": "lookup_order"}}}}
                yield {"contentBlockDelta": {"delta": {"toolUse": {"input": '{"email": "bob@example.com"}'}}}}
                yield {"contentBlockStop": {}}
                yield {"messageStop": {"stopReason": "tool_use"}}
            else:
                yield {"contentBlockDelta": {"delta": {"text": "Your refund was issued on Nov 12."}}}
                yield {"contentBlockStop": {}}
                yield {"messageStop": {"stopReason": "end_turn"}}
            yield {"metadata": {"usage": {"inputTokens": 40, "outputTokens": 12, "totalTokens": 52}, "metrics": {"latencyMs": 5}}}

    return StubModel()


app = BedrockAgentCoreApp()


@app.entrypoint
def invoke(payload):
    model = build_model()
    agent = Agent(name="refund_agent", model=model, tools=[lookup_order], callback_handler=None) if model else \
        Agent(name="refund_agent", tools=[lookup_order], callback_handler=None)
    result = agent(payload.get("prompt", "Hello"))
    return {"result": str(result)}


if __name__ == "__main__":
    app.run(port=int(os.environ.get("PORT", "8080")))
