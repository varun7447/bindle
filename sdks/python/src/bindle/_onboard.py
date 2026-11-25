"""Onboarding helpers shared by the ``bindle`` CLI and the MCP server.

Standard library only. Nothing here imports OpenTelemetry, so ``bindle init``
works before the customer's environment is fully set up. The ingest key is
never printed or returned in full.
"""

from __future__ import annotations

import json
import os
import re
import secrets
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Tuple

USER_AGENT = "bindle-init/0.1"


@dataclass(frozen=True)
class Framework:
    id: str
    label: str
    install: str
    code: str
    packages: Tuple[str, ...]  # dependency names that identify it (normalized)


def _init(agent: str) -> str:
    return (
        "import bindle\n\n"
        "# Reads BINDLE_API_KEY and BINDLE_ENDPOINT from the environment.\n"
        f'bindle.init(agent="{agent}", version="v1", environment="dev")'
    )


# Keep in step with console/lib/snippets.ts (the console's Connect page).
# Order matters: the first match wins, so agent frameworks come before the
# LLM clients they depend on.
FRAMEWORKS: Tuple[Framework, ...] = (
    Framework("langchain", "LangChain / LangGraph", "pip install 'bindle-sdk[langchain]'",
              _init("my-langchain-agent") + "\n\n# Call init() before you build chains or graphs; they are traced automatically.",
              ("langchain", "langchain-core", "langgraph")),
    Framework("openai-agents", "OpenAI Agents SDK", "pip install 'bindle-sdk[openai-agents]'",
              _init("my-openai-agent") + "\n\n# Call init() before Runner.run(); agent, tool and handoff spans are traced.",
              ("openai-agents",)),
    Framework("crewai", "CrewAI", "pip install 'bindle-sdk[crewai]'",
              _init("my-crew") + "\n\n# Call init() before crew.kickoff().", ("crewai",)),
    Framework("llama-index", "LlamaIndex", "pip install 'bindle-sdk[llama-index]'",
              _init("my-llamaindex-agent") + "\n\n# Call init() before building indexes or agents.",
              ("llama-index", "llama-index-core")),
    Framework("strands", "Strands Agents", "pip install bindle-sdk",
              _init("my-strands-agent") + "\n\n# Strands emits OpenTelemetry itself; init() installs the tracer provider it uses.\n"
              "# Call init() before creating the Agent.", ("strands-agents",)),
    Framework("google-adk", "Google ADK", "pip install bindle-sdk",
              _init("my-adk-agent") + "\n\n# ADK emits OpenTelemetry itself; call init() before creating the Runner.\n"
              "# To include prompts and responses on the trace:\n"
              "#   OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental\n"
              "#   OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=SPAN_ONLY",
              ("google-adk",)),
    Framework("agent-framework", "Microsoft Agent Framework", "pip install bindle-sdk",
              _init("my-maf-agent") + "\n\n# Agent Framework emits OpenTelemetry itself; call init() before creating the Agent.\n"
              "# To include prompts and responses: ENABLE_SENSITIVE_DATA=true",
              ("agent-framework", "agent-framework-core")),
    Framework("foundry-agents", "Microsoft Foundry Agent Service", "pip install bindle-sdk azure-core-tracing-opentelemetry",
              _init("my-foundry-agent") + "\n\nfrom azure.core.settings import settings\n"
              "from azure.ai.agents.telemetry import AIAgentsInstrumentor\n\n"
              "settings.tracing_implementation = \"opentelemetry\"\n"
              "AIAgentsInstrumentor().instrument()  # add enable_content_recording=True for prompts and responses",
              ("azure-ai-agents", "azure-ai-projects")),
    Framework("llm-clients", "OpenAI / Anthropic / Bedrock clients", "pip install 'bindle-sdk[openai,anthropic,bedrock]'",
              _init("my-agent") + "\n\n# Raw client calls (openai, anthropic, boto3 bedrock-runtime) are traced as LLM steps.",
              ("openai", "anthropic", "boto3")),
    Framework("python", "Any Python agent", "pip install bindle-sdk",
              _init("my-agent") + "\n\n@bindle.trace_tool\ndef lookup_customer(email: str) -> dict:\n    ...", ()),
)


def framework_by_id(fid: Optional[str]) -> Framework:
    for f in FRAMEWORKS:
        if f.id == fid:
            return f
    return FRAMEWORKS[-1]


def _normalize(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


_DEP_FILES = ("pyproject.toml", "requirements.txt", "requirements-dev.txt", "requirements.in", "Pipfile", "setup.cfg", "setup.py")
_NAME_RE = re.compile(r"""["']?\s*([A-Za-z0-9][A-Za-z0-9._-]*)""")


def project_dependencies(root: Path) -> List[str]:
    """Dependency names mentioned in the project's dependency files. Reads
    only those files in ``root`` (no recursion, no code execution)."""
    names: List[str] = []
    for fname in _DEP_FILES:
        p = root / fname
        try:
            if not p.is_file() or p.stat().st_size > 1 << 20:
                continue
            text = p.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for line in text.splitlines():
            line = line.split("#", 1)[0].strip()
            if not line or line.startswith(("[", "-")):
                continue
            # requirements: "pkg[extra]>=1"; pyproject: '"pkg>=1",' or 'pkg = "^1"'
            m = _NAME_RE.match(line)
            if m:
                names.append(_normalize(m.group(1)))
            for quoted in re.findall(r"""["']([A-Za-z0-9][A-Za-z0-9._-]*)""", line):
                names.append(_normalize(quoted))
    return names


def detect_framework(root: Path) -> Framework:
    deps = set(project_dependencies(root))
    for f in FRAMEWORKS:
        if any(p in deps for p in f.packages):
            return f
    return FRAMEWORKS[-1]


# ---- keys, endpoints, .env -------------------------------------------------

KEY_RE = re.compile(r"^bd_[a-z]+_[0-9a-z]{16,}$")


def mask_key(key: str) -> str:
    return key[:12] + "…" if len(key) > 12 else "…"


def check_endpoint(endpoint: str) -> str:
    """Normalized endpoint; refuses plain HTTP except to this machine, since
    the ingest key travels in a header."""
    u = urllib.parse.urlsplit(endpoint.strip())
    if u.scheme not in ("http", "https") or not u.hostname or u.username or u.password:
        raise ValueError("the endpoint must be an http(s) URL without credentials")
    if u.scheme == "http" and u.hostname not in ("localhost", "127.0.0.1", "::1"):
        raise ValueError("use https: the ingest key would be sent in clear text")
    return urllib.parse.urlunsplit((u.scheme, u.netloc, u.path.rstrip("/"), "", ""))


def console_url(endpoint: str) -> str:
    override = os.environ.get("BINDLE_CONSOLE_URL")
    if override:
        return override.rstrip("/")
    host = urllib.parse.urlsplit(endpoint).hostname or ""
    if host in ("localhost", "127.0.0.1", "::1"):
        return "http://localhost:3000"
    if host.startswith("ingest."):
        return "https://app." + host[len("ingest."):]
    return ""  # unknown: set BINDLE_CONSOLE_URL to get links


def read_env_file(path: Path) -> Dict[str, str]:
    out: Dict[str, str] = {}
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return out
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        k = k.strip()
        if k.startswith("export "):
            k = k[7:].strip()
        out[k] = v.strip().strip("'\"")
    return out


def write_env(root: Path, values: Dict[str, str], force: bool = False) -> Tuple[List[str], List[str]]:
    """Adds values to root/.env (mode 0600). Existing different values are
    kept unless force. Returns (written, kept) key names. Also makes sure
    .env is git-ignored when the project is a git repository."""
    path = root / ".env"
    existing = read_env_file(path)
    written, kept = [], []
    lines = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
    for k, v in values.items():
        if k in existing and existing[k] == v:
            continue
        if k in existing and not force:
            kept.append(k)
            continue
        lines = [ln for ln in lines if not re.match(rf"^\s*(export\s+)?{re.escape(k)}\s*=", ln)]
        lines.append(f"{k}={v}")
        written.append(k)
    if written:
        fd = os.open(str(path), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
    _gitignore_env(root)
    return written, kept


def _gitignore_env(root: Path) -> None:
    if not (root / ".git").exists():
        return
    gi = root / ".gitignore"
    try:
        current = gi.read_text(encoding="utf-8") if gi.exists() else ""
    except OSError:
        return
    if any(ln.strip() in (".env", "/.env", ".env*", "*.env") for ln in current.splitlines()):
        return
    with gi.open("a", encoding="utf-8") as f:
        if current and not current.endswith("\n"):
            f.write("\n")
        f.write("# Bindle ingest key (added by bindle init)\n.env\n")


# ---- talking to Bindle -----------------------------------------------------

@dataclass
class Result:
    ok: bool
    message: str
    detail: Optional[dict] = None


def _request(method: str, url: str, key: str, body: Optional[bytes] = None, timeout: float = 10) -> Tuple[int, bytes]:
    req = urllib.request.Request(url, data=body, method=method, headers={
        "Authorization": "Bearer " + key, "User-Agent": USER_AGENT, "Accept": "application/json",
        **({"Content-Type": "application/json"} if body is not None else {}),
    })
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 (scheme checked)
            return resp.status, resp.read(1 << 16)
    except urllib.error.HTTPError as e:
        return e.code, e.read(1 << 12) if e.fp else b""


def check_connection(endpoint: str, key: str) -> Result:
    """Checks the key against GET /v1/config (no data is sent)."""
    try:
        endpoint = check_endpoint(endpoint)
    except ValueError as e:
        return Result(False, str(e))
    if not KEY_RE.match(key or ""):
        return Result(False, "that doesn't look like a Bindle ingest key (bd_live_…); create one on the console's Ingest keys page")
    try:
        status, body = _request("GET", endpoint + "/v1/config", key)
    except (urllib.error.URLError, OSError, TimeoutError) as e:
        return Result(False, f"can't reach {endpoint}: {getattr(e, 'reason', e)}")
    if status == 200:
        try:
            cfg = json.loads(body)
        except ValueError:
            cfg = {}
        return Result(True, f"connected to {endpoint} with key {mask_key(key)}", {
            "sample_rate": cfg.get("sample_rate"), "capture_content": cfg.get("capture_content"),
        })
    if status in (401, 403):
        return Result(False, f"the key {mask_key(key)} was rejected: it may be revoked, expired or mistyped")
    if status == 429:
        return Result(False, "rate limited; try again in a few seconds")
    return Result(False, f"{endpoint} answered HTTP {status}")


def send_test_trace(endpoint: str, key: str, agent: str = "bindle-init", now_ns: Optional[int] = None) -> Result:
    """Sends one small trace (no customer data) and returns its console link."""
    try:
        endpoint = check_endpoint(endpoint)
    except ValueError as e:
        return Result(False, str(e))
    trace_id, root, child = secrets.token_hex(16), secrets.token_hex(8), secrets.token_hex(8)
    t0 = now_ns if now_ns is not None else time.time_ns()

    def attr(k: str, v: str) -> dict:
        return {"key": k, "value": {"stringValue": v}}

    spans = [
        {"traceId": trace_id, "spanId": root, "name": "bindle.test", "kind": 1,
         "startTimeUnixNano": str(t0), "endTimeUnixNano": str(t0 + 40_000_000),
         "attributes": [attr("bindle.agent.name", agent), attr("bindle.step.type", "agent"), attr("bindle.test", "true")],
         "status": {"code": 1}},
        {"traceId": trace_id, "spanId": child, "parentSpanId": root, "name": "check_connection", "kind": 1,
         "startTimeUnixNano": str(t0 + 5_000_000), "endTimeUnixNano": str(t0 + 30_000_000),
         "attributes": [attr("gen_ai.tool.name", "check_connection"), attr("input.value", "hello from bindle init")],
         "status": {"code": 1}},
    ]
    body = json.dumps({"resourceSpans": [{
        "resource": {"attributes": [attr("service.name", agent), attr("telemetry.sdk.name", "bindle-init")]},
        "scopeSpans": [{"scope": {"name": "bindle.init"}, "spans": spans}],
    }]}).encode()
    try:
        status, _ = _request("POST", endpoint + "/v1/traces", key, body)
    except (urllib.error.URLError, OSError, TimeoutError) as e:
        return Result(False, f"can't reach {endpoint}: {getattr(e, 'reason', e)}")
    if status == 200:
        base = console_url(endpoint)
        if not base:
            return Result(True, f"test trace sent (trace ID {trace_id})", {"trace_id": trace_id})
        link = f"{base}/traces/{trace_id}"
        return Result(True, f"test trace sent: {link}", {"trace_id": trace_id, "url": link})
    if status in (401, 403):
        return Result(False, f"the key {mask_key(key)} was rejected")
    return Result(False, f"ingest answered HTTP {status}")


def setup_guide(f: Framework, endpoint: str = "") -> str:
    env = "export BINDLE_API_KEY=<your ingest key>\nexport BINDLE_ENDPOINT=" + (endpoint or "<your Bindle ingest URL>")
    return (
        f"# {f.label}\n\n1. Install:\n\n    {f.install}\n\n2. Set the key and endpoint (or put them in .env):\n\n"
        + "\n".join("    " + ln for ln in env.splitlines())
        + "\n\n3. Add to your agent's entry point:\n\n"
        + "\n".join("    " + ln if ln else "" for ln in f.code.splitlines())
        + "\n\nServerless: call bindle.flush() before your handler returns. "
        "Without a key or endpoint the SDK does nothing, so this is safe to commit.\n"
    )
