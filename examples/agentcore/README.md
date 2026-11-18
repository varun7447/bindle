# Bindle on Amazon Bedrock AgentCore

A Strands agent packaged for AgentCore Runtime and traced by Bindle. The agent itself is instrumented the same way as anywhere else: `bindle.init()` plus an ingest key. AgentCore only changes how the container is built, where the key comes from, and whether AWS's own observability (ADOT to CloudWatch) runs too.

## What gets traced where

| Source | How it reaches Bindle |
|---|---|
| Your agent's LLM calls, tools and steps | The Bindle SDK in the container exports directly to `BINDLE_ENDPOINT` over HTTPS |
| AgentCore Gateway tool calls, Runtime invocations | AWS writes these service spans to CloudWatch (`aws/spans`). The [CloudWatch span connector](../../deploy/connectors/cloudwatch-spans/) forwards them to Bindle |

When the agent's MCP client sends W3C trace context to the Gateway, both sides share a trace ID and Bindle shows them as one run.

## Choose a setup

**Bindle only.** Set `DISABLE_ADOT_OBSERVABILITY=true` on the runtime so AgentCore's built-in ADOT settings are off, and run `python agent.py` (the Dockerfile's default `CMD`). Do not run `opentelemetry-instrument` in this mode: with ADOT disabled it falls back to exporting to localhost.

**Bindle and CloudWatch.** Leave ADOT on and switch the Dockerfile `CMD` to `opentelemetry-instrument python agent.py`. Bindle detects the ADOT tracer provider and adds its exporter to it, so every span goes to both destinations. Bindle still applies its own redaction and agent name to its copy; CloudWatch gets ADOT's unchanged copy.

Both were tested locally with a stub model: one run per invocation, no runs for `/ping` health checks or Bindle's own settings polls.

## Run it locally (no AWS)

```bash
python -m venv .venv && . .venv/bin/activate
pip install -r examples/agentcore/requirements.txt
set -a; . deploy/local/.env; set +a      # BINDLE_API_KEY and BINDLE_ENDPOINT for the local stack
BINDLE_EXAMPLE_STUB_MODEL=1 PORT=8090 python examples/agentcore/agent.py
curl -s localhost:8090/invocations -H 'content-type: application/json' \
  -d '{"prompt": "Where is my refund? I am bob@example.com"}'
```

Outside a container the AgentCore SDK binds to 127.0.0.1. Open the console and the run appears as `agentcore-refunds`.

## Deploy to AgentCore Runtime

These steps create AWS resources that cost money: an ECR repository, an AgentCore runtime, Bedrock model calls, and a Secrets Manager secret.

1. **Store the ingest key** in Secrets Manager rather than as a runtime environment variable, since environment variables are part of the runtime's configuration:

   ```bash
   aws secretsmanager create-secret --name bindle/ingest-key --secret-string "$BINDLE_API_KEY"
   ```

   Grant the runtime's execution role `secretsmanager:GetSecretValue` on that secret only.

2. **Build and push** the ARM64 image (from this directory):

   ```bash
   docker buildx build --platform linux/arm64 -t "$ECR_REPO:latest" --push .
   ```

3. **Create the runtime:**

   ```bash
   aws bedrock-agentcore-control create-agent-runtime \
     --agent-runtime-name agentcore_refunds \
     --agent-runtime-artifact "{\"containerConfiguration\":{\"containerUri\":\"$ECR_REPO:latest\"}}" \
     --network-configuration '{"networkMode":"PUBLIC"}' \
     --role-arn "$EXECUTION_ROLE_ARN" \
     --environment-variables "BINDLE_ENDPOINT=https://ingest.example.com,BINDLE_API_KEY_SECRET_ARN=$SECRET_ARN,BINDLE_ENVIRONMENT=prod,DISABLE_ADOT_OBSERVABILITY=true"
   ```

   Drop `DISABLE_ADOT_OBSERVABILITY=true` for the Bindle and CloudWatch setup.

### Network access

The SDK sends spans out over HTTPS, so the runtime needs a route to your Bindle ingest endpoint:

- **PUBLIC** network mode (the default) has internet egress. Nothing else is needed.
- **VPC** mode has no egress by default. Add a NAT gateway, or, for a self-hosted Bindle in the same network, point `BINDLE_ENDPOINT` at its private address.

If the endpoint is unreachable the SDK drops spans after its retries; the agent keeps working.

## Gateway and other managed services

AgentCore Gateway runs inside AWS, so it can't load the Bindle SDK. Turn on tracing for the gateway (it requires CloudWatch Transaction Search), then deploy the [CloudWatch span connector](../../deploy/connectors/cloudwatch-spans/) in the same account. It subscribes to `aws/spans` and forwards AgentCore spans to Bindle, where Gateway tool calls appear as tool steps.
