"""Bindle MCP server: lets a coding agent (Claude Code, Cursor, ...) set up
Bindle in the project it's working on.

Model Context Protocol over stdio: newline-delimited JSON-RPC 2.0, standard
library only. Run with ``bindle mcp``. The ingest key comes from
BINDLE_API_KEY (or the project's .env) and is never returned to the model.

Tools:
    bindle_setup_guide       install line and code for a framework
    bindle_detect_framework  which framework this project uses
    bindle_check_connection  is the key valid and the endpoint reachable
    bindle_send_test_trace   send one small trace and get its console link
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import IO, Any, Dict, Optional

from . import _onboard as ob

PROTOCOL_VERSION = "2025-06-18"
SERVER_INFO = {"name": "bindle", "version": "0.1.0"}

_FRAMEWORK_IDS = [f.id for f in ob.FRAMEWORKS]

TOOLS = [
    {
        "name": "bindle_setup_guide",
        "description": "Step-by-step instructions to add Bindle tracing to a Python agent: the pip install line, "
                       "environment variables and the code to add. Use bindle_detect_framework first if unsure.",
        "inputSchema": {"type": "object", "properties": {
            "framework": {"type": "string", "enum": _FRAMEWORK_IDS, "description": "Framework id"},
        }, "additionalProperties": False},
    },
    {
        "name": "bindle_detect_framework",
        "description": "Detect the agent framework from this project's dependency files (pyproject.toml, requirements*.txt).",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
    },
    {
        "name": "bindle_check_connection",
        "description": "Check that BINDLE_API_KEY is valid and the Bindle endpoint is reachable. Sends no trace data.",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
    },
    {
        "name": "bindle_send_test_trace",
        "description": "Send one small test trace (no project data) and return a link to it in the Bindle console.",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
    },
]

_MAX_LINE = 1 << 20


class Server:
    def __init__(self, root: Optional[Path] = None, environ: Optional[Dict[str, str]] = None):
        self.root = (root or Path.cwd()).resolve()
        self.environ = environ if environ is not None else dict(os.environ)

    def _creds(self):
        env_file = ob.read_env_file(self.root / ".env")
        key = self.environ.get("BINDLE_API_KEY") or env_file.get("BINDLE_API_KEY") or ""
        endpoint = self.environ.get("BINDLE_ENDPOINT") or env_file.get("BINDLE_ENDPOINT") or ob.DEFAULT_ENDPOINT
        return key.strip(), endpoint

    def call_tool(self, name: str, args: Dict[str, Any]) -> Dict[str, Any]:
        def text(s: str, error: bool = False) -> Dict[str, Any]:
            return {"content": [{"type": "text", "text": s}], "isError": error}

        if name == "bindle_setup_guide":
            fid = args.get("framework")
            if fid is not None and fid not in _FRAMEWORK_IDS:
                return text(f"Unknown framework. Use one of: {', '.join(_FRAMEWORK_IDS)}", True)
            fw = ob.framework_by_id(fid) if fid else ob.detect_framework(self.root)
            return text(ob.setup_guide(fw, self._creds()[1]))
        if name == "bindle_detect_framework":
            fw = ob.detect_framework(self.root)
            return text(json.dumps({"framework": fw.id, "label": fw.label, "install": fw.install}))
        if name in ("bindle_check_connection", "bindle_send_test_trace"):
            key, endpoint = self._creds()
            if not key:
                return text("BINDLE_API_KEY is not set. Ask the user to create an ingest key in the Bindle console "
                            "(Ingest keys page) and run `bindle init`, or set BINDLE_API_KEY. Never ask them to paste it into chat.", True)
            res = ob.check_connection(endpoint, key) if name == "bindle_check_connection" else ob.send_test_trace(endpoint, key)
            return text(res.message, not res.ok)
        raise KeyError(name)

    def handle(self, msg: Any) -> Optional[Dict[str, Any]]:
        if not isinstance(msg, dict) or msg.get("jsonrpc") != "2.0" or not isinstance(msg.get("method"), str):
            return _error(msg.get("id") if isinstance(msg, dict) else None, -32600, "Invalid Request")
        mid, method, params = msg.get("id"), msg["method"], msg.get("params") or {}
        if "id" not in msg:  # notification
            return None
        if method == "initialize":
            return _ok(mid, {"protocolVersion": PROTOCOL_VERSION, "capabilities": {"tools": {}}, "serverInfo": SERVER_INFO,
                             "instructions": "Use these tools to add Bindle tracing to the user's agent. "
                                             "The ingest key is read from the environment and is never shown."})
        if method == "ping":
            return _ok(mid, {})
        if method == "tools/list":
            return _ok(mid, {"tools": TOOLS})
        if method == "tools/call":
            if not isinstance(params, dict) or not isinstance(params.get("name"), str):
                return _error(mid, -32602, "Invalid params")
            args = params.get("arguments") or {}
            if not isinstance(args, dict):
                return _error(mid, -32602, "arguments must be an object")
            try:
                return _ok(mid, self.call_tool(params["name"], args))
            except KeyError:
                return _error(mid, -32602, f"Unknown tool: {params['name']}")
            except Exception:  # never crash the client's session
                return _ok(mid, {"content": [{"type": "text", "text": "internal error"}], "isError": True})
        return _error(mid, -32601, f"Method not found: {method}")


def _ok(mid: Any, result: Any) -> Dict[str, Any]:
    return {"jsonrpc": "2.0", "id": mid, "result": result}


def _error(mid: Any, code: int, message: str) -> Dict[str, Any]:
    return {"jsonrpc": "2.0", "id": mid, "error": {"code": code, "message": message}}


def serve(stdin: IO[str], stdout: IO[str], server: Optional[Server] = None) -> int:
    server = server or Server()
    for line in stdin:
        if len(line) > _MAX_LINE:
            resp: Optional[Dict[str, Any]] = _error(None, -32600, "message too large")
        elif not line.strip():
            continue
        else:
            try:
                msg = json.loads(line)
            except ValueError:
                resp = _error(None, -32700, "Parse error")
            else:
                if isinstance(msg, list):  # batches aren't part of current MCP
                    resp = _error(None, -32600, "batches are not supported")
                else:
                    resp = server.handle(msg)
        if resp is not None:
            stdout.write(json.dumps(resp) + "\n")
            stdout.flush()
    return 0
