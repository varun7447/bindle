"""``bindle skills prove``: prove a drafted skill on the failures it was
written for, here in your environment, before anyone approves it.

Usage::

    export BINDLE_ENDPOINT=https://ingest.example.com   # your Bindle ingest URL
    export BINDLE_MGMT_KEY=bd_mgmt_...                  # a management key with skills:write

    bindle skills prove skills/retry-refund --agent-name refunds --agent mymodule:run_with_skill \\
        [--dataset dataset.jsonl --checks checks.json] [--report proof.md]

What happens:

1. The lesson named in the skill's metadata (``bindle-lesson``) has
   recorded runs of its failures (cassettes: each run's input and every
   tool call with its recorded result, as Bindle stored them, redacted).
   They are downloaded.
2. Your agent function is called on each failing run's input with the
   skill and the cassette: ``fn(input, *, skill, cassette)``. Answer your
   tools from the recording with ``@cassette.tool`` (see bindle.Cassette),
   so no live system is touched. A run is fixed when the agent finishes,
   every tool call it made was in the recording, and it didn't hit the
   recorded failure again.
3. With ``--dataset`` and ``--checks`` (as for ``bindle eval``), the agent
   runs on the dataset with the skill (``cassette=None``: your normal
   tools), and every check that passed on the recorded run but fails now is
   a regression.
4. The report goes to Bindle, which judges it against the workspace's
   threshold (default: fixes 80% of the replayed failures, no regressions).
   A passing report moves the draft to proven; its owner can then approve
   it. The report lists what replay couldn't cover.

Bindle never runs your agent and never sees a model key.
"""

from __future__ import annotations

import argparse
import re
import sys
import urllib.error
import urllib.parse
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

_NAME_RE = re.compile(r"^name:\s*['\"]?([a-z0-9]+(?:-[a-z0-9]+)*)['\"]?\s*$", re.M)
_LESSON_RE = re.compile(r"^\s+bindle-lesson:\s*['\"]?(les_[0-9a-f]{16})['\"]?\s*$", re.M)


def skill_meta(text: str) -> Tuple[str, str]:
    """The skill's name and lesson ID from its frontmatter."""
    if not text.startswith("---"):
        raise ValueError("not a SKILL.md: it must start with YAML frontmatter")
    end = text.find("\n---", 3)
    front = text[3:end] if end > 0 else ""
    name, lesson = _NAME_RE.search(front), _LESSON_RE.search(front)
    if not name:
        raise ValueError("the SKILL.md has no valid name")
    if not lesson:
        raise ValueError("the SKILL.md has no metadata bindle-lesson (draft it with bindle skills draft)")
    return name.group(1), lesson.group(1)


def replay(fn: Callable, cassettes: List[Dict[str, Any]], skill: str, lesson_tool: str) -> Tuple[List[Dict[str, Any]], List[str]]:
    """Runs fn on each recorded failing run; returns run results and gaps."""
    from ._cassette import Cassette, RecordedToolError, UnrecordedCall

    runs, gaps = [], []
    for data in cassettes:
        try:
            c = Cassette(data, on_missing="error")
        except ValueError as e:
            gaps.append(f"{str(data.get('trace_id', ''))[:12]}: {e}")
            continue
        tid = c.trace_id
        for why in c.incomplete:
            gaps.append(f"{tid}: {why}")
        fixed, covered, detail = False, True, ""
        try:
            fn(c.input, skill=skill, cassette=c)
            fixed = True
        except UnrecordedCall as e:
            covered, detail = False, str(e)
        except RecordedToolError as e:
            detail = f"failed the same way: {e}" if e.tool == lesson_tool else f"a recorded failure: {e}"
        except Exception as e:  # noqa: BLE001 - a crash is a failed replay
            detail = f"{type(e).__name__}: {e}"[:300]
        rep = c.report()
        if rep["missing"]:
            covered, fixed = False, False
            gaps.extend(f"{tid}: tool {t} was called but isn't in the recording" for t in rep["missing"])
            detail = detail or "a tool call wasn't in the recording"
        runs.append({"trace_id": tid, "fixed": fixed and covered, "covered": covered, "detail": detail[:300]})
    return runs, gaps


def eval_regressions(fn: Callable, skill: str, dataset: str, checks: str) -> Tuple[int, int, List[str]]:
    """Runs the eval dataset with the skill; counts checks that passed on
    the recorded run and fail now."""
    import json

    from ._eval import _as_run, _install_collector, evaluate, run_agent

    lines = [json.loads(x) for x in open(dataset, encoding="utf-8") if x.strip()]
    raw = json.load(open(checks, encoding="utf-8"))
    scorers = [s for s in (raw.get("scorers", raw) if isinstance(raw, dict) else raw) if s.get("enabled", True)]
    collector = _install_collector()
    regressions, notes = 0, []
    for i, line in enumerate(lines, 1):
        before = _as_run(line)
        after = run_agent(lambda inp: fn(inp, skill=skill, cassette=None), line, collector)
        for s in scorers:
            if s.get("agent") and line.get("agent") and s["agent"] != line["agent"]:
                continue
            b, a = evaluate(s, before), evaluate(s, after)
            if b.passed and not b.skipped and not a.passed and not a.skipped:
                regressions += 1
                notes.append(f"line {i}: {s['name']}: {a.detail}")
    return len(lines), regressions, notes


def cmd_prove(args: argparse.Namespace) -> int:
    from . import _skills as sk

    p = Path(args.path)
    if p.is_dir():
        p = p / "SKILL.md"
    try:
        text = p.read_text()
        name, lesson = skill_meta(text)
    except (OSError, ValueError) as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    if (args.dataset is None) != (args.checks is None):
        print("✕ --dataset and --checks go together", file=sys.stderr)
        return 2
    conn = sk._connect(args)
    if not conn:
        return 2
    fn = sk._load(args.agent)
    try:
        status, res = sk._call(conn, "GET", f"/v1/lessons/{lesson}/cassettes")
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status != 200:
        return sk._fail(status, res)
    cassettes = res.get("cassettes") or []
    if not cassettes:
        print("✕ the lesson has no recorded runs yet (they are recorded hourly while its failures are kept)", file=sys.stderr)
        return 1
    tool = ""
    status, lres = sk._call(conn, "GET", "/v1/lessons?" + urllib.parse.urlencode({"agent": args.agent_name}))
    if status == 200:
        for item in lres.get("lessons") or []:
            if item.get("id") == lesson:
                tool = item.get("tool") or ""
    runs, gaps = replay(fn, cassettes, text, tool)
    eval_runs, regressions, notes = 0, 0, []
    if args.dataset:
        eval_runs, regressions, notes = eval_regressions(fn, text, args.dataset, args.checks)
    report = {"agent": args.agent_name, "path": f"skills/{name}/SKILL.md", "sha256": sk.digest(text), "lesson": lesson,
              "runs": runs, "eval_runs": eval_runs, "regressions": regressions, "gaps": gaps}
    try:
        status, res = sk._call(conn, "POST", "/v1/skills/proof", report)
    except (urllib.error.URLError, OSError) as e:
        print(f"✕ can't reach Bindle: {getattr(e, 'reason', e)}", file=sys.stderr)
        return 1
    if status == 409:
        print("✕ the skill in Bindle isn't this text: push it first (bindle skills push), then prove it", file=sys.stderr)
        return 1
    if status != 200:
        return sk._fail(status, res)
    v = res.get("verdict") or {}
    fixed = sum(1 for r in runs if r["fixed"])
    md = [f"## Proof for {name}: {'passed' if v.get('passed') else 'failed'}", "",
          f"{v.get('reason', '')}.", "",
          f"- Replayed {len(runs)} recorded failures: {fixed} fixed.",
          f"- Eval runs: {eval_runs}, regressions: {regressions}." if args.dataset else "- No eval dataset given.",
          f"- Status in Bindle: {res.get('status')}" + (" (proven: its owner can approve it)" if res.get("proven") else "")]
    if gaps:
        md += ["", "What replay couldn't cover:"] + [f"- {g}" for g in gaps[:50]]
    failed = [r for r in runs if not r["fixed"]]
    if failed:
        md += ["", "Not fixed:"] + [f"- {r['trace_id'][:12]}: {r['detail'] or 'not fixed'}" for r in failed[:50]]
    if notes:
        md += ["", "Regressions:"] + [f"- {n}" for n in notes[:50]]
    text_out = "\n".join(md) + "\n"
    print(text_out)
    if args.report:
        Path(args.report).write_text(text_out)
    return 0 if v.get("passed") else 1


def add_prove(ps: Any, common: Callable[[argparse.ArgumentParser], None]) -> None:
    sp = ps.add_parser("prove", help="replay the lesson's failures with the skill, here, and send the proof report",
                       description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sp.add_argument("path", help="SKILL.md or the skill's folder")
    sp.add_argument("--agent-name", required=True, help="the agent the skill is for")
    sp.add_argument("--agent", required=True, help="module:function called as fn(input, *, skill, cassette)")
    sp.add_argument("--dataset", help="eval dataset (JSON Lines), as for bindle eval")
    sp.add_argument("--checks", help="checks file, as for bindle eval")
    sp.add_argument("--report", help="also write the Markdown report here")
    common(sp)
    sp.set_defaults(fn=cmd_prove)


__all__ = ["add_prove", "skill_meta", "replay", "eval_regressions"]
