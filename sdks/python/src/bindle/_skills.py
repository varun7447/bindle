"""``bindle skills`` and ``bindle vault``: turn repeated failures into
skills, and keep an agent's knowledge vault in your repository.

Usage::

    export BINDLE_ENDPOINT=https://ingest.example.com   # your Bindle ingest URL
    export BINDLE_MGMT_KEY=bd_mgmt_...                  # a management key (Settings -> Keys)

    bindle skills lessons [--agent refund-agent]
    bindle skills draft --lesson les_0123456789abcdef --writer mymodule:draft_skill [--out skills]
    bindle skills push skills/retry-lookup-order --agent refund-agent

    bindle vault pull   --agent refund-agent --dir vault/refund-agent
    bindle vault push   --agent refund-agent --dir vault/refund-agent
    bindle vault export --agent refund-agent --dir /tmp/refund-agent-vault

Drafting runs here, in your environment. ``--writer module:function`` is your
own code (like ``bindle eval --judge``): it is called with the lesson pack
(a dict of redacted, normalized data: the error pattern, likely cause and a
few failing steps) and returns ``{"description": ..., "body": ...}``
(optionally ``"name"``), typically by calling your model with your key.
Bindle never sees a model key. The CLI wraps the result as a SKILL.md in the
Agent Skills format (agentskills.io) with Bindle metadata, and checks it.

``skills push`` stores the skill as a draft. Bindle redacts it, scans it for
injected instructions, hidden text and secrets, and rejects it if it names a
tool or data class outside the agent's passport. A person approves it later
in the console; nothing reaches an agent until then.

``vault pull`` and ``vault push`` sync an agent's notes with a folder, so
skills can be reviewed in pull requests. A ``.bindle-vault.json`` file in the
folder records the version each file was last synced at; a note changed on
both sides is reported as a conflict and left alone.

These need the learning loop to be enabled for the workspace, and a
management key with the ``skills:write`` (skills) or ``vault:sync`` (vault)
scope.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import urllib.error
import urllib.parse
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

from . import _onboard as ob
from ._passport import MGMT_PREFIX, _creds, _send

STATE_FILE = ".bindle-vault.json"
MAX_NAME = 64
MAX_DESCRIPTION = 1024
MAX_LINES = 500
_NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
_PATH_RE = re.compile(r"^[a-z0-9][a-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*\.md$")
_LESSON_RE = re.compile(r"^les_[0-9a-f]{16}$")


class SkillError(ValueError):
    """A skill that doesn't follow the Agent Skills format."""


# ---- shared ------------------------------------------------------------------


def _connect(args: argparse.Namespace) -> Optional[Tuple[str, str]]:
    # .env is read from the current folder (--dir is the vault folder for vault commands).
    key, endpoint = _creds(argparse.Namespace(key=args.key, endpoint=args.endpoint, dir="."))
    if not key.startswith(MGMT_PREFIX):
        print("✕ A management key (bd_mgmt_...) is required: set BINDLE_MGMT_KEY or pass --key.", file=sys.stderr)
        return None
    if not endpoint:
        print("✕ No endpoint: set BINDLE_ENDPOINT to your Bindle ingest URL or pass --endpoint.", file=sys.stderr)
        return None
    try:
        return key, ob.check_endpoint(endpoint)
    except ValueError as e:
        print(f"✕ {e}", file=sys.stderr)
        return None


def _call(conn: Tuple[str, str], method: str, path: str, body: Optional[Dict[str, Any]] = None) -> Tuple[int, Dict[str, Any]]:
    key, endpoint = conn
    data = json.dumps(body).encode() if body is not None else None
    return _send(endpoint, key, method, path, data)  # type: ignore[arg-type]


def _fail(status: int, res: Dict[str, Any]) -> int:
    msg = res.get("error") or f"HTTP {status}"
    if isinstance(msg, dict):
        msg = msg.get("message") or str(msg)
    if status == 404 and not res.get("error"):
        msg = "the learning loop isn't enabled for this workspace"
    print(f"✕ {msg}", file=sys.stderr)
    check = res.get("passport_check")
    if check and check.get("reason"):
        print(f"  Passport: {check['reason']}", file=sys.stderr)
    return 1


def digest(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def _load(spec: str) -> Callable:
    from ._eval import _load as load

    return load(spec)


# ---- SKILL.md ----------------------------------------------------------------


def _yaml_str(v: str) -> str:
    """A single-line YAML scalar, quoted when it needs to be."""
    v = " ".join(v.split())
    if v == "" or re.search(r"[:#\[\]{}&*!|>'\"%@`,]", v) or v[0] in "-?" or v.lower() in ("true", "false", "null", "yes", "no"):
        return json.dumps(v, ensure_ascii=False)
    return v


def skill_name(pack: Dict[str, Any], given: Optional[str] = None) -> str:
    """A valid skill name: the writer's, or one made from the lesson."""
    raw = given or f"{pack.get('tool') or pack.get('step_name') or 'step'}-{pack.get('cause') or 'error'}"
    name = re.sub(r"[^a-z0-9]+", "-", raw.lower()).strip("-")
    name = re.sub(r"-{2,}", "-", name)[:MAX_NAME].strip("-")
    if not name or not _NAME_RE.match(name):
        raise SkillError(f"can't make a valid skill name from {raw!r}")
    return name


def build_skill(pack: Dict[str, Any], draft: Dict[str, Any]) -> Tuple[str, str]:
    """Wraps a writer's result as a SKILL.md. Returns (name, text)."""
    if not isinstance(draft, dict):
        raise SkillError("the writer must return a dict with description and body")
    description = " ".join(str(draft.get("description") or "").split())
    body = str(draft.get("body") or "").strip()
    if not description:
        raise SkillError("the writer returned no description")
    if len(description) > MAX_DESCRIPTION:
        raise SkillError(f"the description is longer than {MAX_DESCRIPTION} characters")
    if not body:
        raise SkillError("the writer returned no body")
    name = skill_name(pack, draft.get("name"))
    lines = ["---", f"name: {name}", f"description: {_yaml_str(description)}", "metadata:",
             f"  bindle-lesson: {pack.get('lesson', '')}", f"  bindle-agent: {_yaml_str(str(pack.get('agent', '')))}",
             "  bindle-status: draft", '  bindle-version: "1"', "---", "", body, "", "## Evidence", "",
             f"Drafted from a lesson: {pack.get('failures', '?')} failures in {pack.get('runs', '?')} runs, "
             f"error `{pack.get('error_pattern', '')}` (likely cause: {pack.get('cause_text', pack.get('cause', ''))}).", ""]
    for e in pack.get("evidence") or []:
        if re.match(r"^[0-9a-f]{32}$", str(e.get("trace_id", ""))):
            lines.append(f"- [[run:{e['trace_id']}]] {e.get('name', '')}")
    text = "\n".join(lines).rstrip() + "\n"
    if text.count("\n") + 1 > MAX_LINES:
        raise SkillError(f"a SKILL.md should stay under {MAX_LINES} lines; ask the writer for a shorter body")
    return name, text


# ---- skills ------------------------------------------------------------------


def cmd_lessons(args: argparse.Namespace) -> int:
    conn = _connect(args)
    if not conn:
        return 2
    q = "?" + urllib.parse.urlencode({"agent": args.agent}) if args.agent else ""
    try:
        status, res = _call(conn, "GET", "/v1/lessons" + q)
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status != 200:
        return _fail(status, res)
    lessons = res.get("lessons") or []
    if not lessons:
        print("No lessons yet: no failure repeats often enough.")
    for l in lessons:
        print(f"{l['id']}  {l['agent']}: {l['title']}")
    return 0


def cmd_draft(args: argparse.Namespace) -> int:
    if not _LESSON_RE.match(args.lesson):
        print("✕ --lesson must be a lesson ID (les_ followed by 16 hex digits)", file=sys.stderr)
        return 2
    conn = _connect(args)
    if not conn:
        return 2
    writer = _load(args.writer)
    try:
        status, pack = _call(conn, "GET", f"/v1/lessons/{args.lesson}/pack")
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status != 200:
        return _fail(status, pack)
    try:
        draft = writer(pack)  # the customer's code, with the customer's model
        name, text = build_skill(pack, draft)
    except SkillError as e:
        print(f"✕ {e}", file=sys.stderr)
        return 1
    except Exception as e:  # noqa: BLE001 - report the writer's own failure plainly
        print(f"✕ the writer failed: {type(e).__name__}: {e}", file=sys.stderr)
        return 1
    out = Path(args.out) / name
    out.mkdir(parents=True, exist_ok=True)
    (out / "SKILL.md").write_text(text)
    print(f"✓ Drafted {out / 'SKILL.md'}. Review it, then: bindle skills push {out} --agent {pack.get('agent', '<agent>')}")
    return 0


def cmd_push(args: argparse.Namespace) -> int:
    p = Path(args.path)
    if p.is_dir():
        p = p / "SKILL.md"
    try:
        content = p.read_text()
    except OSError as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    conn = _connect(args)
    if not conn:
        return 2
    try:
        status, res = _call(conn, "POST", "/v1/skills", {"agent": args.agent, "content": content})
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status not in (200, 201):
        return _fail(status, res)
    note = " (unchanged)" if res.get("unchanged") else ""
    print(f"✓ {res['path']}: version {res['version']}, {res.get('status', 'draft')}{note}")
    scan = res.get("scan") or {}
    if scan.get("flagged"):
        print(f"  The security scan flagged it ({', '.join(scan.get('reasons') or [])}); a security reviewer must clear it before approval.")
    check = res.get("passport_check") or {}
    if check and not check.get("passport"):
        print("  The agent has no passport: push one before this skill can be approved.")
    return 0


# ---- vault -------------------------------------------------------------------


def _safe(base: Path, rel: str) -> Optional[Path]:
    if not _PATH_RE.match(rel) or ".." in rel:
        return None
    p = (base / rel).resolve()
    try:
        p.relative_to(base.resolve())
    except ValueError:
        return None
    return p


def _state(d: Path, agent: str) -> Dict[str, str]:
    f = d / STATE_FILE
    if not f.exists():
        return {}
    try:
        s = json.loads(f.read_text())
    except ValueError:
        return {}
    if s.get("agent") != agent:
        raise SkillError(f"{f} belongs to agent {s.get('agent')!r}, not {agent!r}")
    return dict(s.get("files") or {})


def _save_state(d: Path, agent: str, files: Dict[str, str]) -> None:
    (d / STATE_FILE).write_text(json.dumps({"agent": agent, "files": dict(sorted(files.items()))}, indent=2) + "\n")


def _list(conn: Tuple[str, str], agent: str) -> Tuple[int, Dict[str, Any]]:
    return _call(conn, "GET", "/v1/vault?" + urllib.parse.urlencode({"agent": agent}))


def cmd_pull(args: argparse.Namespace) -> int:
    conn = _connect(args)
    if not conn:
        return 2
    d = Path(args.dir)
    try:
        state = _state(d, args.agent)
        status, res = _list(conn, args.agent)
    except SkillError as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status != 200:
        return _fail(status, res)
    d.mkdir(parents=True, exist_ok=True)
    written, conflicts = 0, []
    for f in res.get("files") or []:
        p = _safe(d, f["path"])
        if p is None:
            print(f"  Skipped {f['path']!r}: not a safe path", file=sys.stderr)
            continue
        recorded = state.get(f["path"], "")
        if p.exists():
            local = digest(p.read_text())
            if local == f["sha256"]:
                state[f["path"]] = f["sha256"]
                continue
            if local != recorded:  # changed here, and different from Bindle's
                conflicts.append(f["path"])
                continue
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(f["content"])
        state[f["path"]] = f["sha256"]
        written += 1
    _save_state(d, args.agent, state)
    print(f"✓ Pulled {written} note(s) into {d}")
    for c in conflicts:
        print(f"  Conflict: {c} changed both here and in Bindle; merge by hand, then push", file=sys.stderr)
    return 1 if conflicts else 0


def cmd_vault_push(args: argparse.Namespace) -> int:
    conn = _connect(args)
    if not conn:
        return 2
    d = Path(args.dir)
    try:
        state = _state(d, args.agent)
    except SkillError as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    pushed, conflicts, failed = 0, [], 0
    for p in sorted(d.rglob("*.md")):
        rel = p.relative_to(d).as_posix()
        if any(part.startswith(".") for part in p.relative_to(d).parts) or not _PATH_RE.match(rel):
            continue
        content = p.read_text()
        base = state.get(rel, "")
        if digest(content) == base:
            continue
        try:
            status, res = _call(conn, "PUT", "/v1/vault", {"agent": args.agent, "path": rel, "content": content, "base_sha256": base})
        except (urllib.error.URLError, OSError) as e:
            print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
            return 1
        if status == 409:
            conflicts.append(rel)
            continue
        if status not in (200, 201):
            print(f"  {rel}:", end=" ", file=sys.stderr)
            _fail(status, res)
            failed += 1
            continue
        state[rel] = res["sha256"]
        pushed += 1
        if res["sha256"] != digest(content):
            print(f"  {rel}: Bindle redacted part of it; run `bindle vault pull` to get the stored text")
    _save_state(d, args.agent, state)
    print(f"✓ Pushed {pushed} note(s)")
    for c in conflicts:
        print(f"  Conflict: {c} changed in Bindle since your last pull; pull, merge, then push", file=sys.stderr)
    return 1 if conflicts or failed else 0


def cmd_export(args: argparse.Namespace) -> int:
    conn = _connect(args)
    if not conn:
        return 2
    d = Path(args.dir)
    if d.exists() and any(d.iterdir()):
        print(f"✕ {d} isn't empty; export into a new folder", file=sys.stderr)
        return 2
    try:
        status, res = _list(conn, args.agent)
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status != 200:
        return _fail(status, res)
    n = 0
    for f in res.get("files") or []:
        p = _safe(d, f["path"])
        if p is None:
            continue
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(f["content"])
        n += 1
    print(f"✓ Exported {n} note(s) to {d}")
    return 0


def add_parser(sub: Any) -> None:
    def common(sp: argparse.ArgumentParser) -> None:
        sp.add_argument("--key", help="management key (default: BINDLE_MGMT_KEY or .env)")
        sp.add_argument("--endpoint", help="Bindle ingest URL (default: BINDLE_ENDPOINT or .env)")

    p = sub.add_parser("skills", help="lessons from repeated failures, and skills drafted from them",
                       description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ps = p.add_subparsers(dest="action", required=True)
    sp = ps.add_parser("lessons", help="list lessons")
    sp.add_argument("--agent", help="only this agent's lessons")
    common(sp)
    sp.set_defaults(fn=cmd_lessons)
    sp = ps.add_parser("draft", help="draft a SKILL.md from a lesson with your own model (runs here)")
    sp.add_argument("--lesson", required=True, help="lesson ID (bindle skills lessons)")
    sp.add_argument("--writer", required=True, help="module:function called with the lesson pack; returns {description, body}")
    sp.add_argument("--out", default="skills", help="folder for the drafted skill (default: skills)")
    common(sp)
    sp.set_defaults(fn=cmd_draft)
    sp = ps.add_parser("push", help="push a SKILL.md (or its folder) as a draft")
    sp.add_argument("path", help="SKILL.md or the skill's folder")
    sp.add_argument("--agent", required=True, help="the agent the skill is for")
    common(sp)
    sp.set_defaults(fn=cmd_push)

    v = sub.add_parser("vault", help="sync an agent's knowledge vault with a folder",
                       description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    vs = v.add_subparsers(dest="action", required=True)
    for action, fn, text in (("pull", cmd_pull, "fetch notes into the folder"),
                             ("push", cmd_vault_push, "send changed notes from the folder"),
                             ("export", cmd_export, "write every note into a new, empty folder")):
        sp = vs.add_parser(action, help=text)
        sp.add_argument("--agent", required=True, help="the agent whose vault this is")
        sp.add_argument("--dir", required=True, help="the folder")
        common(sp)
        sp.set_defaults(fn=fn)


__all__ = ["add_parser", "build_skill", "skill_name", "SkillError", "digest"]
