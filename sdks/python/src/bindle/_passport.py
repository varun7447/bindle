"""``bindle passport``: validate and push agent passports, and export them as
AWS Dogwood policies.

A passport is a JSON file (version ``bindle.passport/v1``) declaring what one
agent may do: tools, data classes, budgets, per-tool rate limits and release
rights. Bindle compares every step with it and reports drift; nothing is
blocked.

Usage::

    export BINDLE_ENDPOINT=https://ingest.example.com   # your Bindle ingest URL
    export BINDLE_MGMT_KEY=bd_mgmt_...                  # a management key (Settings -> Keys)
    bindle passport validate passport.json
    bindle passport push passport.json
    bindle passport export --format dogwood passport.json \\
        --gateway-arn arn:aws:bedrock-agentcore:us-west-2:111122223333:gateway/my-gw \\
        --target OrdersAPI [--tools tools.json]

validate and push go to ingest, the public endpoint agents already reach; the
management key decides the workspace. Ingest keys (bd_live_) are refused, so
an agent's own key can't change its passport. Pushing the same document
again doesn't create a new version.

export works offline. It compiles a passport into Cedar-compatible Dogwood
policies for Amazon Bedrock AgentCore Policy, using only syntax from the
AgentCore Policy developer guide: one permit per allowed tool, a count-based
temporal forbid per rate limit, and a forbid for release tools when the
passport gives no release rights. Budgets stay with Bindle's agent limits.
Data classes are not exported (a marked placeholder says so). Experimental:
review the policies, and run them in LOG_ONLY mode first.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

from . import _onboard as ob

MGMT_PREFIX = "bd_mgmt_"
VERSION = "bindle.passport/v1"
_MAX_BYTES = 64 << 10

# AgentCore Policy quotas for temporal policies (developer guide, "Temporal policies").
MAX_TEMPORAL_POLICIES = 20
MAX_WINDOW_SECONDS = 24 * 3600

_DEFAULT_RELEASE_TOOLS = ["deploy*", "release*", "publish*", "merge*", "git_push", "*config_push*"]
_WINDOW_RE = re.compile(r"^([1-9][0-9]{0,4})(s|m|h)$")
_TARGET_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$")
_ARN_RE = re.compile(r"^arn:aws[a-z-]*:bedrock-agentcore:[a-z0-9-]+:[0-9]{12}:gateway/[A-Za-z0-9_-]+$")


class ExportError(ValueError):
    """A passport that can't be compiled as asked."""


# ---- validate / push ---------------------------------------------------------


def _creds(args: argparse.Namespace) -> Tuple[str, str]:
    env_file = ob.read_env_file(Path(args.dir).resolve() / ".env")
    key = args.key or os.environ.get("BINDLE_MGMT_KEY") or env_file.get("BINDLE_MGMT_KEY") or ""
    endpoint = args.endpoint or os.environ.get("BINDLE_ENDPOINT") or env_file.get("BINDLE_ENDPOINT") or ""
    return key.strip(), endpoint.strip()


def _read(path: str) -> bytes:
    data = Path(path).read_bytes()
    if len(data) > _MAX_BYTES:
        raise ValueError("a passport must be at most 64 KiB")
    json.loads(data)  # fail early on a file that isn't JSON
    return data


def _send(endpoint: str, key: str, method: str, path: str, body: bytes) -> Tuple[int, Dict[str, Any]]:
    req = urllib.request.Request(endpoint + path, data=body, method=method,
                                 headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json",
                                          "User-Agent": "bindle-cli"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:  # noqa: S310 - endpoint checked by check_endpoint
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}")
        except ValueError:
            return e.code, {}


def cmd_send(args: argparse.Namespace) -> int:
    key, endpoint = _creds(args)
    if not key.startswith(MGMT_PREFIX):
        print("✕ A management key (bd_mgmt_...) is required: set BINDLE_MGMT_KEY or pass --key. "
              "Create one under Settings -> Keys; ingest keys can't change passports.", file=sys.stderr)
        return 2
    if not endpoint:
        print("✕ No endpoint: set BINDLE_ENDPOINT to your Bindle ingest URL or pass --endpoint.", file=sys.stderr)
        return 2
    try:
        endpoint = ob.check_endpoint(endpoint)
        body = _read(args.file)
    except (OSError, ValueError) as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    push = args.action == "push"
    try:
        status, res = _send(endpoint, key, "PUT" if push else "POST", "/v1/passports" if push else "/v1/passports/validate", body)
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach {endpoint}: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status in (200, 201):
        if not push:
            print(f"✓ Valid passport for {res.get('agent')}")
        elif res.get("unchanged"):
            print(f"✓ {res.get('agent')}: already at version {res.get('version')} (unchanged)")
        else:
            print(f"✓ {res.get('agent')}: pushed version {res.get('version')}")
        return 0
    msg = res.get("error") or f"HTTP {status}"
    if status == 404:
        msg = "passports aren't enabled for this workspace"
    print(f"✕ {msg}", file=sys.stderr)
    return 1


# ---- Dogwood export ----------------------------------------------------------


def _window(w: str) -> str:
    """A passport window in the units AgentCore's examples use (m, h)."""
    m = _WINDOW_RE.match(w)
    if not m:
        raise ExportError(f"window {w!r} is not like 30s, 5m or 1h")
    n, unit = int(m.group(1)), m.group(2)
    seconds = n * {"s": 1, "m": 60, "h": 3600}[unit]
    if seconds > MAX_WINDOW_SECONDS:
        raise ExportError(f"window {w} is over AgentCore's 24-hour maximum")
    if unit == "s":
        if seconds % 60:
            raise ExportError(f"window {w}: only minute and hour windows are exported (the seconds unit isn't verified)")
        return f"{seconds // 60}m"
    return f"{n}{unit}"


def _tool_names(path: Optional[str]) -> Optional[List[str]]:
    """Tool names from a JSON file: a list of names, or an MCP tools/list
    result ({"tools": [{"name": ...}]})."""
    if not path:
        return None
    data = json.loads(Path(path).read_text())
    if isinstance(data, dict):
        data = data.get("tools", [])
    names = []
    for t in data:
        name = t.get("name") if isinstance(t, dict) else t
        if isinstance(name, str) and name:
            names.append(name)
    return sorted(set(names))


def _expand(patterns: List[str], tools: Optional[List[str]]) -> Tuple[List[str], List[str]]:
    """Concrete tool names for patterns, and the patterns that couldn't be expanded."""
    names, skipped = [], []
    for p in patterns:
        if "*" not in p:
            names.append(p)
        elif tools is None:
            skipped.append(p)
        else:
            # Passport wildcards: * only; everything else is literal.
            rx = re.compile("^" + ".*".join(re.escape(part) for part in p.split("*")) + "$")
            matched = [t for t in tools if rx.match(t)]
            names.extend(matched)
            if not matched:
                skipped.append(p)
    return sorted(set(names)), skipped


def _action(target: str, tool: str) -> str:
    return f'AgentCore::Action::"{target}___{tool}"'


def _id(prefix: str, tool: str) -> str:
    return prefix + re.sub(r"[^A-Za-z0-9_]", "_", tool)


def export_dogwood(passport: Dict[str, Any], gateway_arn: str, target: str, tools: Optional[List[str]] = None) -> str:
    """Compile a passport into Dogwood policy text. Raises ExportError."""
    if passport.get("version") != VERSION:
        raise ExportError(f"version must be {VERSION!r}")
    agent = passport.get("agent")
    if not isinstance(agent, str) or not agent:
        raise ExportError("the passport has no agent")
    if not _ARN_RE.match(gateway_arn):
        raise ExportError("--gateway-arn must be an AgentCore gateway ARN (arn:aws:bedrock-agentcore:<region>:<account>:gateway/<id>)")
    if not _TARGET_RE.match(target):
        raise ExportError("--target must be the gateway target name (letters, digits, - and _)")
    for t in tools or []:
        if '"' in t or "\\" in t:
            raise ExportError(f"tool name {t!r} can't be used in a policy")
    gw = f'AgentCore::Gateway::"{gateway_arn}"'
    out: List[str] = [
        f"// Dogwood policies for agent {json.dumps(agent)}, compiled by `bindle passport export` from {VERSION}.",
        f"// Owner: {passport.get('owner', '')}. Experimental: review before use, and start in LOG_ONLY mode.",
        "// Dogwood is default-deny and forbid wins: a call is allowed only if a permit applies and no forbid does.",
        "",
    ]

    tools_section = passport.get("tools")
    if tools_section is None:
        out += ["// The passport doesn't restrict tools, so every tool on this gateway is permitted.",
                "permit (", "    principal,", "    action,", f"    resource == {gw}", ");", ""]
    else:
        allowed, skipped = _expand(list(tools_section.get("allow") or []), tools)
        if not allowed:
            out += ["// The passport allows no tools: no permit, so every call is denied.", ""]
        for name in allowed:
            out += [f'@id("{_id("bindle_allow_", name)}")', "permit (", "    principal,", f"    action == {_action(target, name)},",
                    f"    resource == {gw}", ");", ""]
        for p in skipped:
            out += [f"// SKIPPED: tools.allow pattern {json.dumps(p)} matched no tool name. Pass --tools with the gateway's tool list to expand it.", ""]

    limits = list(passport.get("limits") or [])
    temporal = 0
    for lim in limits:
        tool, mx, window = lim.get("tool", ""), lim.get("max"), lim.get("window", "")
        if "*" in tool:
            out += [f"// SKIPPED: rate limit on pattern {json.dumps(tool)}: a combined count over several tools isn't exported.", ""]
            continue
        if not isinstance(mx, int) or mx < 1:
            raise ExportError(f"limit on {tool}: max must be a positive whole number")
        within = _window(window)
        temporal += 1
        if temporal > MAX_TEMPORAL_POLICIES:
            raise ExportError(f"more than {MAX_TEMPORAL_POLICIES} rate limits: AgentCore allows {MAX_TEMPORAL_POLICIES} temporal policies per policy engine")
        out += [
            f"// At most {mx} call(s) to {tool} in any {within} of a policy session.",
            "// Not a security control: the caller supplies the session ID and can reset the count by starting a new",
            "// session. Use it to shape behaviour in a cooperative session (for example, to stop a looping agent).",
            f'@id("{_id("bindle_rate_", tool)}")',
            "forbid (",
            "    principal,",
            f"    action == {_action(target, tool)},",
            f"    resource == {gw}",
            ")",
            "when temporal {",
            "    exists (n: Long).",
            "        (count for (t: Timepoint).",
            f"            where (formerly within {within} ({_action(target, tool)}::request{{ eventResource: resource }} && tp(t)))) == n",
            f"        && n > {mx}",
            "};",
            "",
        ]

    if passport.get("release_rights") == "none":
        release, skipped = _expand(list(passport.get("release_tools") or _DEFAULT_RELEASE_TOOLS), tools)
        for name in release:
            out += [f"// The passport gives no release rights: {name} is a release action.", f'@id("{_id("bindle_no_release_", name)}")',
                    "forbid (", "    principal,", f"    action == {_action(target, name)},", f"    resource == {gw}", ");", ""]
        for p in skipped:
            out += [f"// SKIPPED: release tool pattern {json.dumps(p)} matched no tool name (pass --tools to expand it).", ""]

    if passport.get("data_classes"):
        out += [
            "// PLACEHOLDER (not exported): data_classes (allow / egress_block). Dogwood guardrail conditions",
            "// (`when guardrails { ... }`) may be able to express this, but their syntax wasn't verified, so Bindle",
            "// doesn't generate them. Bindle keeps reporting data-class drift either way.",
            "",
        ]
    if passport.get("budget"):
        out += ["// Budgets (per run, per day) are not exported: they stay with Bindle's agent limits (check_limits()).", ""]
    return "\n".join(out).rstrip() + "\n"


def cmd_export(args: argparse.Namespace) -> int:
    if args.format != "dogwood":
        print("✕ the only export format is dogwood", file=sys.stderr)
        return 2
    try:
        passport = json.loads(_read(args.file))
        text = export_dogwood(passport, args.gateway_arn, args.target, _tool_names(args.tools))
    except (OSError, ValueError) as e:
        print(f"✕ {e}", file=sys.stderr)
        return 1
    if args.out:
        Path(args.out).write_text(text)
        print(f"✓ Wrote {args.out}")
    else:
        sys.stdout.write(text)
    return 0


def add_parser(sub: Any) -> None:
    p = sub.add_parser("passport", help="validate, push or export an agent passport (JSON)",
                       description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ps = p.add_subparsers(dest="action", required=True)
    for action, text in (("validate", "check a passport with Bindle (stores nothing)"),
                         ("push", "push a passport as the agent's next version")):
        sp = ps.add_parser(action, help=text)
        sp.add_argument("file", help="passport JSON file")
        sp.add_argument("--key", help="management key (default: BINDLE_MGMT_KEY or .env)")
        sp.add_argument("--endpoint", help="Bindle ingest URL (default: BINDLE_ENDPOINT or .env)")
        sp.add_argument("--dir", default=".", help="directory with .env (default: current)")
        sp.set_defaults(fn=cmd_send)
    sp = ps.add_parser("export", help="compile a passport into policies (offline)")
    sp.add_argument("file", help="passport JSON file")
    sp.add_argument("--format", required=True, choices=["dogwood"], help="policy language")
    sp.add_argument("--gateway-arn", required=True, help="AgentCore gateway ARN the policies apply to")
    sp.add_argument("--target", required=True, help="gateway target name; actions are <target>___<tool>")
    sp.add_argument("--tools", help="JSON list of tool names (or an MCP tools/list result) to expand * patterns")
    sp.add_argument("--out", help="write to this file instead of stdout")
    sp.set_defaults(fn=cmd_export)


__all__ = ["export_dogwood", "ExportError", "add_parser"]
