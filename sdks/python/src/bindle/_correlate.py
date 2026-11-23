"""Correlation keys: link a run to business events reported later.

``bindle.correlate("ticket_id", "T-123")`` during a run lets a business
system report, days later and without the run's trace ID, that ticket T-123
was reopened. Only a digest of the key and value leaves this process; Bindle
binds it to the workspace before storing it, and never sees the raw value.
"""

from __future__ import annotations

import hashlib
import re
import threading
from collections import OrderedDict
from typing import Optional

ATTR_PREFIX = "bindle.correlation."
MAX_KEYS = 8
MAX_VALUE_BYTES = 256
_KEY_RE = re.compile(r"[a-z][a-z0-9_]{0,31}")
_MAX_TRACKED_RUNS = 1024


def valid_key(key: object) -> bool:
    return isinstance(key, str) and _KEY_RE.fullmatch(key) is not None


def valid_value(value: object) -> bool:
    return isinstance(value, str) and 0 < len(value.encode("utf-8")) <= MAX_VALUE_BYTES


def digest(key: str, value: str) -> str:
    """What is sent for a key and value (the same as Bindle's Go code)."""
    h = hashlib.sha256(("bindle.correlation.v1\x00" + key + "\x00" + value).encode("utf-8"))
    return "v1:" + h.hexdigest()


class RunKeys:
    """Keys already set per run, so a run carries at most MAX_KEYS."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._runs: "OrderedDict[int, set]" = OrderedDict()

    def admit(self, trace_id: int, key: str) -> bool:
        with self._lock:
            keys = self._runs.get(trace_id)
            if keys is None:
                keys = set()
                self._runs[trace_id] = keys
                while len(self._runs) > _MAX_TRACKED_RUNS:
                    self._runs.popitem(last=False)
            if key in keys:
                return True
            if len(keys) >= MAX_KEYS:
                return False
            keys.add(key)
            return True


def attribute(key: str, value: object) -> Optional[tuple]:
    """(attribute name, digest) for a valid key and value, else None. Numbers
    are accepted and correlated as their decimal text."""
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        value = str(value)
    if not valid_key(key) or not valid_value(value):
        return None
    return ATTR_PREFIX + key, digest(key, value)  # type: ignore[arg-type]
