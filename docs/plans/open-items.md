# Open items

Work that is known but not yet done. Larger efforts have their own plans in this folder.

- **Run on AWS.** The CDK stack synthesizes and the Docker stack is verified end to end; a first deployment to a real account is still pending.
- **CloudWatch span connector against real data.** [`deploy/connectors/cloudwatch-spans/`](../../deploy/connectors/cloudwatch-spans/) is tested with fixtures, not yet with real `aws/spans` data.
- **Google Gemini prices.** Needs effective-dated entries (Google schedules price changes ahead of time) and a Google Cloud API key for the Cloud Billing Catalog API.
- **Normalizer contract tests per framework release.** Pin a recorded span set per framework version so a framework upgrade that changes its attributes fails CI.
- **Forwarding cloud evaluation results.** Azure AI Foundry continuous evaluation (Application Insights) and AgentCore Evaluations (CloudWatch) results; needs cloud accounts to verify. Both already arrive if sent as OpenTelemetry `gen_ai.evaluation.result` events.
- **Hosted LLM gateway** ([plan](llm-gateway.md)) and **SOC 2** ([plan](soc2.md)).
