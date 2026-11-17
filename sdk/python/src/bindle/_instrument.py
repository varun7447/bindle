"""Auto-instrumentation: detect which agent frameworks are installed and switch
on a matching instrumentor if one is available.

Frameworks that already emit OpenTelemetry (for example Strands) need nothing
here: they use the global tracer provider that ``bindle.init()`` sets up.

The module and class names below come from the OpenInference project. Verify
them against the versions you pin before each release.
"""

from __future__ import annotations

import importlib
import importlib.util
import logging
from typing import Dict, List, Tuple, Union

log = logging.getLogger("bindle")

# name -> (framework module to detect, instrumentor module, instrumentor class, pip extra)
INSTRUMENTORS: Dict[str, Tuple[str, str, str, str]] = {
    "langchain": ("langchain_core", "openinference.instrumentation.langchain", "LangChainInstrumentor", "langchain"),
    "openai-agents": ("agents", "openinference.instrumentation.openai_agents", "OpenAIAgentsInstrumentor", "openai-agents"),
    "crewai": ("crewai", "openinference.instrumentation.crewai", "CrewAIInstrumentor", "crewai"),
    "llama-index": ("llama_index.core", "openinference.instrumentation.llama_index", "LlamaIndexInstrumentor", "llama-index"),
    "openai": ("openai", "openinference.instrumentation.openai", "OpenAIInstrumentor", "openai"),
    "anthropic": ("anthropic", "openinference.instrumentation.anthropic", "AnthropicInstrumentor", "anthropic"),
    "bedrock": ("boto3", "openinference.instrumentation.bedrock", "BedrockInstrumentor", "bedrock"),
}

# Emit OpenTelemetry themselves (checked against strands-agents 1.65,
# google-adk 2.14 and agent-framework 1.26).
NATIVE_OTEL = {"strands": "strands", "google-adk": "google.adk", "agent-framework": "agent_framework"}


def _installed(module: str) -> bool:
    try:
        return importlib.util.find_spec(module) is not None
    except (ImportError, ValueError):
        return False


def detect() -> List[str]:
    """Names of supported frameworks that are importable in this process."""
    found = [name for name, (mod, *_rest) in INSTRUMENTORS.items() if _installed(mod)]
    found += [name for name, mod in NATIVE_OTEL.items() if _installed(mod)]
    return found


def instrument(tracer_provider, which: Union[bool, List[str]]) -> List[str]:
    """Enable instrumentors. Returns the names that were switched on.
    Never raises: a broken instrumentor must not break the customer's agent."""
    if which is False:
        return []
    names = detect() if which is True else list(which)
    enabled: List[str] = []
    for name in names:
        if name in NATIVE_OTEL:
            enabled.append(name)
            continue
        spec = INSTRUMENTORS.get(name)
        if spec is None:
            log.warning("bindle: unknown framework %r; skipping", name)
            continue
        _, inst_module, inst_class, extra = spec
        if not _installed(inst_module):
            log.info("bindle: %s detected; run `pip install 'bindle-sdk[%s]'` for full tracing", name, extra)
            continue
        try:
            cls = getattr(importlib.import_module(inst_module), inst_class)
            inst = cls()
            if not getattr(inst, "is_instrumented_by_opentelemetry", False):
                inst.instrument(tracer_provider=tracer_provider)
            enabled.append(name)
        except Exception as exc:  # noqa: BLE001
            log.warning("bindle: could not instrument %s: %s", name, exc)
    return enabled
