# @bindle-ai/sdk

Observability for AI agents in Node.js (20+). Trace every step your agent takes (LLM calls, tool calls, decisions, handoffs) with one line. Same behaviour as the Python SDK: redaction on by default, console settings as a ceiling, and it never throws into or blocks your agent.

```bash
npm install @bindle-ai/sdk
```

```ts
import * as bindle from '@bindle-ai/sdk';

// Reads BINDLE_API_KEY and BINDLE_ENDPOINT from the environment.
bindle.init({ agent: 'support-triage', version: 'v12', environment: 'prod' });
```

Without a key, `init()` warns once and does nothing, so the line is safe to commit. Frameworks and libraries that emit OpenTelemetry use the tracer provider `init()` registers.

## Custom agent code

```ts
const lookupCustomer = bindle.traceTool(async function lookupCustomer(email: string) {
  return crm.find(email);
});

await bindle.withSession(conversationId, () =>
  bindle.step('plan', { type: 'agent' }, async () => {
    const customer = await lookupCustomer(email);
    bindle.decision('route', ['crm', 'kb'], 'crm', 'needs account data');
    bindle.handoff('human', { reason: 'refund over limit' });
  }),
);
```

- `step(name, { type, attributes }, fn)` traces a block (sync or async); errors are recorded and rethrown.
- `traceTool(fn)` and `traceStep(fn, { name, type })` wrap functions; arguments become the input and the result the output.
- `withSession(id, fn)` tags every step inside `fn` with a session ID.

## Serverless

Await `bindle.flush()` before your handler returns.

## Already using OpenTelemetry?

If your app registers its own tracer provider, `init()` leaves it alone and returns false. Add Bindle's processor to your provider instead:

```ts
new TracerProvider({ spanProcessors: [bindle.spanProcessor({ agent: 'my-agent' })!, ...yours] });
```

Or send OTLP/HTTP directly to `$BINDLE_ENDPOINT/v1/traces` with `authorization: Bearer $BINDLE_API_KEY`.

## Configuration

| Option | Environment | Default |
|---|---|---|
| `apiKey` | `BINDLE_API_KEY` | none (SDK disabled) |
| `endpoint` | `BINDLE_ENDPOINT` | `https://ingest.bindle.ai` (https required, except localhost) |
| `agent`, `version`, `environment` | `BINDLE_AGENT`, `BINDLE_AGENT_VERSION`, `BINDLE_ENVIRONMENT` | none |
| `sampleRate` | `BINDLE_SAMPLE_RATE` | `1` |
| `redact` | `BINDLE_REDACT` | `true` |
| `captureContent` | `BINDLE_CAPTURE_CONTENT` | `true` |
| `redactPatterns` | | `{}` (`NAME: regex`) |
| `remoteConfig` | `BINDLE_REMOTE_CONFIG` | `true` |

Sampling uses the same rule as the Python SDK and Bindle's ingest, so all three keep the same traces. Console settings (sample rate, content capture, extra redaction patterns) are polled from `/v1/config`; code can only be stricter.
