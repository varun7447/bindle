# CloudWatch span connector

Managed AWS services such as Amazon Bedrock AgentCore Gateway can't run the Bindle SDK. They write OpenTelemetry spans to CloudWatch Logs instead, in the `aws/spans` log group. This connector runs in **your** AWS account and forwards those spans to Bindle, so Gateway tool calls, Policy decisions and Identity token exchanges appear in the same run as your agent's own steps.

```
AgentCore Gateway / Policy / Identity ──▶ CloudWatch Logs (aws/spans)
                                              │ subscription filter: { $.name = "AgentCore.*" }
                                              ▼
                                     Lambda (this connector) ──HTTPS──▶ Bindle ingest /v1/traces
```

Spans join your agent's run when they share its trace ID, which happens when the agent's MCP client sends W3C trace context to the Gateway (the MCP spec carries it in `_meta`). Otherwise they appear as separate runs. Nothing in the stack listens on the network. The function only makes outbound HTTPS calls to your Bindle endpoint, using an ingest key it reads from Secrets Manager.

## How spans appear in Bindle

| AgentCore span | Bindle step |
|---|---|
| `AgentCore.Gateway.InvokeTool` (SERVER) | Tool call, named by `tool.name` |
| `AgentCore.Gateway.InvokeTool` (CLIENT, the target) | Other, so each call counts once |
| `AgentCore.Policy.*` | Guardrail |
| Other `AgentCore.*` spans | Other |

The framework shows as "Amazon Bedrock AgentCore".

## Before you deploy

1. Turn on CloudWatch Transaction Search, and turn on tracing for each Gateway (or other AgentCore resource) you want to see. Spans then appear in `aws/spans`.
2. Store a Bindle ingest key in Secrets Manager:

   ```bash
   aws secretsmanager create-secret --name bindle/ingest-key --secret-string "$BINDLE_API_KEY"
   ```

3. A log group accepts at most two subscription filters. Check that `aws/spans` has a free slot:

   ```bash
   aws logs describe-subscription-filters --log-group-name aws/spans
   ```

## Deploy

Deploying creates paid resources: a Lambda function, its log group, and Lambda invocations for each batch of matching spans. CloudWatch Logs charges for `aws/spans` apply whether or not the connector is deployed.

```bash
cd deploy/connectors/cloudwatch-spans
npm ci
npx cdk deploy \
  -c bindleEndpoint=https://ingest.example.com \
  -c apiKeySecretArn=arn:aws:secretsmanager:REGION:ACCOUNT:secret:bindle/ingest-key-XXXXXX
```

Options (all `-c` context values):

| Name | Default | Meaning |
|---|---|---|
| `bindleEndpoint` | required | Bindle ingest base URL, https only |
| `apiKeySecretArn` | required | Complete ARN of the secret holding the ingest key |
| `logGroupName` | `aws/spans` | Log group to subscribe to |
| `spanNamePrefixes` | `AgentCore.` | Comma-separated span name prefixes to forward |

Deploy one stack per region that has AgentCore resources.

## Behaviour

- **Retries.** Ingest responses 429 and 5xx are retried three times, honouring `Retry-After`. After that the invocation fails and Lambda retries the batch. Bindle writes spans by trace and span ID, so a retried batch doesn't create duplicates.
- **Format tolerance.** AWS documents `aws/spans` attributes but not every field of the record. The converter accepts nested or flat attributes, OTLP key/value lists, string or numeric kinds and status codes, X-Ray or hex trace IDs, and end times or durations. Records without valid IDs are skipped.
- **Redaction.** Bindle's server-side redaction applies to forwarded spans like any others.

## Status

Unit tested, and tested end to end against a local Bindle stack with sample records. It has not yet been run against real `aws/spans` data. The first deployment should confirm the record format: look for forwarded runs in the console, and for `forwarded: 0` in the function's logs while spans are arriving.

## Tests

```bash
cd forwarder && python3 -m unittest -v test_handler
```
