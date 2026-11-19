"""Replay a recorded run with its tool outputs.

Download a run from the Bindle console (Run details → Download for replay)
and re-run your agent on the same input, with every tool call answered from
the recording instead of the real tool. Change a prompt, a model or the
agent's code and see how it behaves on a real failure, without side effects
(no emails sent, no refunds issued, no live APIs called)::

    cassette = bindle.Cassette.load("run-0af7….cassette.json")

    @cassette.tool
    def lookup_order(order_id: str) -> dict: ...

    answer = my_agent(cassette.input)

Calls are matched by tool name and arguments (key order ignored). Repeated
identical calls get the recorded results in order. A call with arguments
the run never used gets the next unused recorded result for that tool
(``match_order=False`` turns this off). A call to a tool the run never made
raises ``UnrecordedCall``, unless the cassette was loaded with
``on_missing="call"``, which calls the real tool instead. A tool call that
failed in the recording raises ``RecordedToolError`` with its message.
"""

from __future__ import annotations

import functools
import inspect
import json
import threading
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional, Union

from ._limits import canonical_args

FORMAT = "bindle.cassette"


class UnrecordedCall(LookupError):
    """A tool call that the recorded run never made."""

    def __init__(self, tool: str, arguments: str):
        super().__init__(f"the recorded run never called {tool!r}"
                         + (f" (arguments {arguments[:200]})" if arguments else "")
                         + "; load the cassette with on_missing='call' to call the real tool")
        self.tool, self.arguments = tool, arguments


class RecordedToolError(RuntimeError):
    """The recorded tool call failed; raised again on replay."""

    def __init__(self, tool: str, message: str):
        super().__init__(message or f"{tool} failed in the recorded run")
        self.tool = tool


@dataclass(frozen=True)
class RecordedCall:
    index: int
    tool: str
    arguments: str  # canonical
    result: str
    status: str
    error: str
    step_id: str


def _decode(text: str) -> Any:
    try:
        return json.loads(text)
    except (TypeError, ValueError):
        return text


class Cassette:
    """A recorded run: its input, final output and tool calls."""

    def __init__(self, data: Dict[str, Any], *, on_missing: str = "error", match_order: bool = True):
        if not isinstance(data, dict) or data.get("format") != FORMAT:
            raise ValueError("not a Bindle cassette (download one from Run details → Download for replay)")
        if int(data.get("version") or 0) > 1:
            raise ValueError(f"cassette version {data.get('version')} is newer than this SDK; upgrade bindle-sdk")
        if on_missing not in ("error", "call"):
            raise ValueError("on_missing must be 'error' or 'call'")
        self.trace_id: str = str(data.get("trace_id") or "")
        self.agent: str = str(data.get("agent") or "")
        self.agent_version: str = str(data.get("agent_version") or "")
        self.input: str = str(data.get("input") or "")
        self.output: str = str(data.get("output") or "")
        self.incomplete: List[str] = [str(x) for x in data.get("incomplete") or []]
        self.on_missing, self.match_order = on_missing, match_order
        calls = []
        for i, c in enumerate(data.get("tool_calls") or []):
            if not isinstance(c, dict) or not c.get("tool"):
                continue
            calls.append(RecordedCall(i, str(c["tool"]), canonical_args(c.get("arguments") or ""),
                                      str(c.get("result") or ""), str(c.get("status") or ""),
                                      str(c.get("error") or ""), str(c.get("step_id") or "")))
        self.calls: List[RecordedCall] = calls
        self._used: set = set()
        self._log: List[Dict[str, Any]] = []
        self._lock = threading.Lock()

    @classmethod
    def load(cls, path: str, *, on_missing: str = "error", match_order: bool = True) -> "Cassette":
        """Reads a cassette downloaded from the console."""
        with open(path, encoding="utf-8") as f:
            return cls(json.load(f), on_missing=on_missing, match_order=match_order)

    # -- matching ------------------------------------------------------------

    def _take(self, tool: str, candidates: List[str]) -> Optional[RecordedCall]:
        with self._lock:
            for key in candidates:
                for c in self.calls:
                    if c.index not in self._used and c.tool == tool and c.arguments == key:
                        self._used.add(c.index)
                        self._log.append({"tool": tool, "match": "arguments", "step_id": c.step_id})
                        return c
            if self.match_order:
                for c in self.calls:
                    if c.index not in self._used and c.tool == tool:
                        self._used.add(c.index)
                        self._log.append({"tool": tool, "match": "order", "step_id": c.step_id})
                        return c
            self._log.append({"tool": tool, "match": "missing", "step_id": ""})
            return None

    def _answer(self, c: RecordedCall, raw: bool) -> Any:
        if c.status == "failure":
            raise RecordedToolError(c.tool, c.error)
        return c.result if raw else _decode(c.result)

    def result(self, tool: str, arguments: Union[str, Dict[str, Any], None] = None, *, raw: bool = False) -> Any:
        """The recorded result of calling ``tool`` with ``arguments``.

        Results that are JSON are decoded unless ``raw=True``. Raises
        ``UnrecordedCall`` when the run never made a matching call, and
        ``RecordedToolError`` when the recorded call failed.
        """
        key = canonical_args(arguments if arguments is not None else "")
        c = self._take(tool, [key])
        if c is None:
            raise UnrecordedCall(tool, key)
        return self._answer(c, raw)

    def tool(self, _fn: Optional[Callable] = None, *, name: Optional[str] = None, raw: bool = False):
        """Decorator: answer calls to this function from the recording.

        The tool name defaults to the function's name; pass ``name=`` when
        the run recorded it under another name. Works on sync and async
        functions, and stacks with ``@bindle.trace_tool``.
        """

        def decorate(fn: Callable) -> Callable:
            tool_name = name or getattr(fn, "__name__", "tool")
            try:
                sig: Optional[inspect.Signature] = inspect.signature(fn)
            except (TypeError, ValueError):
                sig = None

            def keys(args, kwargs) -> List[str]:
                out = []
                if sig is not None:
                    try:
                        bound = sig.bind(*args, **kwargs)
                        bound.apply_defaults()
                        out.append(canonical_args(dict(bound.arguments)))
                        given = sig.bind(*args, **kwargs)
                        out.append(canonical_args(dict(given.arguments)))
                    except TypeError:
                        pass
                if kwargs and not args:
                    out.append(canonical_args(kwargs))
                if len(args) == 1 and not kwargs:
                    out.append(canonical_args(args[0]))
                # bindle.trace_tool records {"args": [...], "kwargs": {...}}
                out.append(canonical_args({"args": list(args), "kwargs": kwargs}))
                return out

            if inspect.iscoroutinefunction(fn):
                @functools.wraps(fn)
                async def async_wrapper(*args, **kwargs):
                    c = self._take(tool_name, keys(args, kwargs))
                    if c is None:
                        if self.on_missing == "call":
                            return await fn(*args, **kwargs)
                        raise UnrecordedCall(tool_name, keys(args, kwargs)[0])
                    return self._answer(c, raw)

                return async_wrapper

            @functools.wraps(fn)
            def wrapper(*args, **kwargs):
                c = self._take(tool_name, keys(args, kwargs))
                if c is None:
                    if self.on_missing == "call":
                        return fn(*args, **kwargs)
                    raise UnrecordedCall(tool_name, keys(args, kwargs)[0])
                return self._answer(c, raw)

            return wrapper

        return decorate(_fn) if _fn is not None else decorate

    # -- after the replay ----------------------------------------------------

    def unused(self) -> List[RecordedCall]:
        """Recorded calls the replay never made (the new run took another path)."""
        with self._lock:
            return [c for c in self.calls if c.index not in self._used]

    def report(self) -> Dict[str, Any]:
        """How the replay used the recording: calls matched by arguments,
        matched by order, missing, and recorded calls never made."""
        with self._lock:
            log = list(self._log)
        return {
            "trace_id": self.trace_id,
            "recorded_calls": len(self.calls),
            "matched_by_arguments": sum(1 for e in log if e["match"] == "arguments"),
            "matched_by_order": sum(1 for e in log if e["match"] == "order"),
            "missing": [e["tool"] for e in log if e["match"] == "missing"],
            "unused": [c.tool for c in self.unused()],
            "incomplete": list(self.incomplete),
        }

    def reset(self) -> None:
        """Forget which recorded calls were used, to replay again."""
        with self._lock:
            self._used.clear()
            self._log.clear()
