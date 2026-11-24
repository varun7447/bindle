"""The ``bindle`` command.

    bindle init     detect the framework, save the key to .env, check the
                    connection, send a test trace, print the code to add
    bindle check    check the key and endpoint (sends nothing)
    bindle mcp      run the Bindle MCP server on stdio (for coding agents)
"""

from __future__ import annotations

import argparse
import getpass
import os
import sys
from pathlib import Path
from typing import List, Optional

from . import _onboard as ob


def _resolve(args: argparse.Namespace, root: Path) -> tuple:
    env_file = ob.read_env_file(root / ".env")
    key = args.key or os.environ.get("BINDLE_API_KEY") or env_file.get("BINDLE_API_KEY") or ""
    endpoint = args.endpoint or os.environ.get("BINDLE_ENDPOINT") or env_file.get("BINDLE_ENDPOINT") or ob.DEFAULT_ENDPOINT
    return key.strip(), endpoint


def cmd_init(args: argparse.Namespace) -> int:
    root = Path(args.dir).resolve()
    key, endpoint = _resolve(args, root)
    try:
        endpoint = ob.check_endpoint(endpoint)
    except ValueError as e:
        print(f"✕ {e}", file=sys.stderr)
        return 2
    fw = ob.framework_by_id(args.framework) if args.framework else ob.detect_framework(root)
    print(f"• Framework: {fw.label}" + ("" if args.framework else " (detected)"))

    if not key:
        if not sys.stdin.isatty():
            print("✕ No ingest key. Pass --key, set BINDLE_API_KEY, or run interactively.", file=sys.stderr)
            return 2
        print(f"  Create a key on {ob.console_url(endpoint)}/keys, then paste it here (input is hidden).")
        key = getpass.getpass("  Ingest key: ").strip()

    res = ob.check_connection(endpoint, key)
    print(("✓ " if res.ok else "✕ ") + res.message)
    if not res.ok:
        return 1
    if res.detail and res.detail.get("capture_content") is False:
        print("  Note: content capture is off for this workspace (SDK settings), so prompts and outputs won't be stored.")

    if not args.no_env:
        values = {"BINDLE_API_KEY": key}
        if endpoint != ob.DEFAULT_ENDPOINT:
            values["BINDLE_ENDPOINT"] = endpoint
        written, kept = ob.write_env(root, values, force=args.force)
        if written:
            print(f"✓ Saved {', '.join(written)} to {root / '.env'} (readable only by you)")
        for k in kept:
            print(f"  Kept the existing {k} in .env; rerun with --force to replace it.")

    if not args.no_test:
        t = ob.send_test_trace(endpoint, key)
        print(("✓ " if t.ok else "✕ ") + t.message)
        if not t.ok:
            return 1

    print()
    print(ob.setup_guide(fw, endpoint))
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    key, endpoint = _resolve(args, Path(args.dir).resolve())
    if not key:
        print("✕ No ingest key. Pass --key or set BINDLE_API_KEY.", file=sys.stderr)
        return 2
    res = ob.check_connection(endpoint, key)
    print(("✓ " if res.ok else "✕ ") + res.message)
    return 0 if res.ok else 1


def cmd_eval(args: argparse.Namespace) -> int:
    import bindle

    from . import _eval

    bindle.init(instrument=False)  # a no-op without BINDLE_API_KEY; with it, runs and judge scores are recorded
    return _eval.run(args.dataset, args.checks, args.agent, args.judge, args.min_pass_rate, args.max_drop, args.report)


def cmd_mcp(_: argparse.Namespace) -> int:
    from . import _mcp

    return _mcp.serve(sys.stdin, sys.stdout)


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(prog="bindle", description="Bindle: observability for AI agents.")
    sub = p.add_subparsers(dest="cmd", required=True)

    def common(sp: argparse.ArgumentParser) -> None:
        sp.add_argument("--key", help="ingest key (default: BINDLE_API_KEY or .env)")
        sp.add_argument("--endpoint", help=f"ingest endpoint (default: BINDLE_ENDPOINT or {ob.DEFAULT_ENDPOINT})")
        sp.add_argument("--dir", default=".", help="project directory (default: current)")

    sp = sub.add_parser("init", help="connect this project to Bindle")
    common(sp)
    sp.add_argument("--framework", choices=[f.id for f in ob.FRAMEWORKS], help="skip detection")
    sp.add_argument("--no-env", action="store_true", help="don't write .env")
    sp.add_argument("--no-test", action="store_true", help="don't send a test trace")
    sp.add_argument("--force", action="store_true", help="replace existing values in .env")
    sp.set_defaults(fn=cmd_init)

    sp = sub.add_parser("check", help="check the key and endpoint")
    common(sp)
    sp.set_defaults(fn=cmd_check)

    sp = sub.add_parser("eval", help="grade an agent on an exported dataset (for CI)")
    sp.add_argument("--dataset", required=True, help="dataset exported from Bindle (JSONL)")
    sp.add_argument("--checks", required=True, help="checks file downloaded from Bindle (JSON)")
    sp.add_argument("--agent", required=True, help="module:function that answers one input")
    sp.add_argument("--judge", help="module:function that grades an answer (runs here, with your keys)")
    sp.add_argument("--min-pass-rate", type=float, help="fail below this pass rate (0-1)")
    sp.add_argument("--max-drop", type=float, default=0.0, help="fail if the pass rate drops more than this below the baseline (0-1)")
    sp.add_argument("--report", help="also write the Markdown report here")
    sp.set_defaults(fn=cmd_eval)

    sp = sub.add_parser("mcp", help="run the Bindle MCP server on stdio")
    sp.set_defaults(fn=cmd_mcp)

    from . import _passport

    _passport.add_parser(sub)

    from . import _skills

    _skills.add_parser(sub)

    args = p.parse_args(argv)
    return args.fn(args)


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
