# Bringing evaluation scores into Bindle

Bindle shows scores next to the run they grade, per agent version (Agents → Checks and scores). Scores reach Bindle in three ways:

| Source | How |
|---|---|
| Your own code or CI | `bindle.score(name, value, passed=…, trace_id=…)` and `bindle.feedback(…)` in the Python and TypeScript SDKs; `bindle eval` for datasets in CI |
| Any OpenTelemetry evaluator | The standard **`gen_ai.evaluation.result`** event, as a span event on the evaluated span or as an OTLP log record sent to `{BINDLE_ENDPOINT}/v1/logs` with the evaluated span's trace and span IDs |
| Bindle's own checks | Evals → Checks, run on datasets and on recent runs |

## The OpenTelemetry evaluation event

The GenAI semantic conventions define `gen_ai.evaluation.result` (status: development). Bindle reads:

| Attribute | Used as |
|---|---|
| `gen_ai.evaluation.name` | score name (required) |
| `gen_ai.evaluation.score.value` | numeric score |
| `gen_ai.evaluation.score.label` | pass/fail when it is a common word (`pass`, `fail`, `true`, `false`, `relevant`, …); other labels are kept as an attribute |
| `gen_ai.evaluation.explanation` | shown as the score's output (redacted, and dropped when content capture is off) |

Log records that aren't evaluation events are ignored: Bindle doesn't store logs. Records sent to `/v1/logs` are treated like scores sent from CI. Their sender's service name and version never relabel the run they grade.

## Microsoft Foundry (Azure AI Foundry)

Foundry's continuous evaluation samples live agent runs and scores them with built-in evaluators such as Relevance, Fluency and Coherence. It writes each result to the project's Application Insights as a `gen_ai.evaluation.result` record (`traces` table), with the run's thread and run IDs.

- **Agent traces**: instrument the agent with the Bindle SDK. See Settings → Connect → Microsoft Foundry Agent Service.
- **Evaluation scores**: when you run Foundry's evaluators from your own code, export their OpenTelemetry events to Bindle's `/v1/logs` with your ingest key, with the run's trace context. The scores then appear on the run. Continuous-evaluation results that Foundry itself writes to Application Insights stay there. Forwarding them needs an export from Application Insights (for example a data export to Event Hubs and a small function posting OTLP to Bindle). That isn't built yet because it needs an Azure subscription to verify.

## Google Vertex AI Agent Engine

Agent Engine's evaluation is batch: you run an evaluation task over a dataset from your code, with trajectory metrics (`trajectory_exact_match`, `trajectory_in_order_match`, `trajectory_any_order_match`, `trajectory_precision`, `trajectory_recall`, `trajectory_single_tool_use`) plus response metrics. Results return to your code rather than to a continuous service. To see them in Bindle, send one `bindle.score()` per row with the row's trace ID, the same way as for any judge. Agent Engine traces go to Cloud Trace. To trace into Bindle instead, use the Bindle SDK in the agent (as for AgentCore Runtime).

## Amazon Bedrock AgentCore

AgentCore Evaluations writes its results to CloudWatch, both as log records and as metrics. The CloudWatch span connector (`deploy/connectors/cloudwatch-spans`) forwards AgentCore service spans today. Forwarding evaluation results is the next step for it, and needs one check of the record format against a real AWS account.
