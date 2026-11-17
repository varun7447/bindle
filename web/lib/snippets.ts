// Connect-page snippets. Only APIs the Python SDK actually has (see
// sdk/python/README.md and _instrument.py): bindle.init() plus the pip extras
// that map to OpenInference instrumentors, and Strands' native OpenTelemetry.

export interface Framework {
  id: string;
  label: string;
  install: string;
  code: string;
  note?: string;
}

const init = (agent: string) => `import bindle

# Reads BINDLE_API_KEY and BINDLE_ENDPOINT from the environment.
bindle.init(agent="${agent}", version="v1", environment="dev")`;

export const FRAMEWORKS: Framework[] = [
  {
    id: 'python',
    label: 'Any Python agent',
    install: 'pip install bindle-sdk',
    code: `${init('my-agent')}

@bindle.trace_tool
def lookup_customer(email: str) -> dict:
    ...

with bindle.step("plan", type="decision"):
    bindle.decision("route", options=["crm", "kb"], chosen="crm", rationale="needs account data")`,
    note: 'Serverless: call bindle.flush() before your handler returns.',
  },
  {
    id: 'langchain',
    label: 'LangChain / LangGraph',
    install: "pip install 'bindle-sdk[langchain]'",
    code: `${init('my-langchain-agent')}

# Call init() before you build chains or graphs; they are traced automatically.`,
  },
  {
    id: 'openai-agents',
    label: 'OpenAI Agents SDK',
    install: "pip install 'bindle-sdk[openai-agents]'",
    code: `${init('my-openai-agent')}

# Call init() before Runner.run(); agent, tool and handoff spans are traced.`,
  },
  {
    id: 'crewai',
    label: 'CrewAI',
    install: "pip install 'bindle-sdk[crewai]'",
    code: `${init('my-crew')}

# Call init() before crew.kickoff().`,
  },
  {
    id: 'llama-index',
    label: 'LlamaIndex',
    install: "pip install 'bindle-sdk[llama-index]'",
    code: `${init('my-llamaindex-agent')}

# Call init() before building indexes or agents.`,
  },
  {
    id: 'strands',
    label: 'Strands Agents',
    install: 'pip install bindle-sdk',
    code: `${init('my-strands-agent')}

# Strands emits OpenTelemetry itself; init() installs the tracer provider it uses.
# Call init() before creating the Agent.`,
  },
  {
    id: 'llm-clients',
    label: 'OpenAI / Anthropic / Bedrock clients',
    install: "pip install 'bindle-sdk[openai,anthropic,bedrock]'",
    code: `${init('my-agent')}

# Raw client calls (openai, anthropic, boto3 bedrock-runtime) are traced as LLM steps.`,
  },
  {
    id: 'otel',
    label: 'Any OpenTelemetry app',
    install: '# No Bindle SDK needed: use your OTLP/HTTP exporter',
    code: `export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT={{ENDPOINT}}
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer $BINDLE_API_KEY"`,
    note: 'Bindle accepts OTLP/HTTP traces (protobuf or JSON, optionally gzip) at /v1/traces.',
  },
];

export function envBlock(endpoint: string): string {
  return `export BINDLE_API_KEY=<your ingest key>
export BINDLE_ENDPOINT=${endpoint}`;
}

export function frameworkById(id: string | undefined): Framework {
  return FRAMEWORKS.find((f) => f.id === id) ?? FRAMEWORKS[0]!;
}
