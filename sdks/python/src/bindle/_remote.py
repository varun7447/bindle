"""Remote configuration: settings a team changes in the Bindle console
(sampling, content capture, extra redaction patterns) without redeploying.

A daemon thread polls ``GET {endpoint}/v1/config`` with the ingest key. The
console's settings are a ceiling: code can only be stricter, so the
effective sample rate is the lower of the two, content is captured only if
both allow it, and remote redaction patterns add to the local ones. Ingest
enforces the same settings, so this is about saving bandwidth and keeping
data in-process, not about trust.

Never raises into the host application, never blocks it, and backs off when
Bindle is unreachable.
"""

from __future__ import annotations

import json
import logging
import random
import re
import threading
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Callable, Dict, Optional

log = logging.getLogger("bindle")

try:
    from opentelemetry import context as _otel_context
except Exception:  # noqa: BLE001
    _otel_context = None


def _suppress_instrumentation():
    """Mark the current context so instrumentations skip it; returns a token."""
    key = getattr(_otel_context, "_SUPPRESS_INSTRUMENTATION_KEY", None) if _otel_context else None
    if key is None:
        return None
    return _otel_context.attach(_otel_context.set_value(key, True))

_MIN_POLL, _MAX_POLL, _MAX_BACKOFF = 30.0, 3600.0, 600.0
_MAX_BODY = 64 * 1024
_NAME_RE = re.compile(r"^[A-Z][A-Z0-9_]{0,31}$")


@dataclass(frozen=True)
class RemoteSettings:
    version: int = 0
    sample_rate: float = 1.0
    capture_content: bool = True
    redact_patterns: Dict[str, str] = field(default_factory=dict)
    poll_interval: float = 60.0
    limits: tuple = ()
    over_budget: tuple = ()


def parse(body: bytes) -> Optional[RemoteSettings]:
    """Parses and validates a /v1/config response; None if unusable."""
    try:
        d = json.loads(body.decode("utf-8"))
        rate = float(d["sample_rate"])
        capture = d["capture_content"]
        version = int(d.get("version", 0))
        if not (0.0 <= rate <= 1.0) or not isinstance(capture, bool):
            return None
        patterns: Dict[str, str] = {}
        for p in d.get("redact_patterns") or []:
            name, expr = p.get("name"), p.get("pattern")
            if isinstance(name, str) and isinstance(expr, str) and _NAME_RE.match(name) and 0 < len(expr) <= 200:
                patterns[name] = expr
        poll = float(d.get("poll_interval_seconds", 60))
        from ._limits import parse_limits

        over = tuple(a for a in (d.get("over_budget") or []) if isinstance(a, str))
        return RemoteSettings(version, rate, capture, patterns, min(max(poll, _MIN_POLL), _MAX_POLL),
                              parse_limits(d.get("limits")), over)
    except Exception:  # noqa: BLE001 - malformed config is ignored, never fatal
        return None


class Poller(threading.Thread):
    """Polls remote settings and calls on_update(settings) when they change."""

    def __init__(self, url: str, api_key: str, on_update: Callable[[RemoteSettings], None],
                 user_agent: str, timeout: float = 5.0, opener=None):
        super().__init__(name="bindle-remote-config", daemon=True)
        self._url, self._key, self._on_update = url, api_key, on_update
        self._ua, self._timeout = user_agent, timeout
        self._etag: Optional[str] = None
        self._interval = 60.0
        self._failures = 0
        self._stop = threading.Event()
        self._open = opener or urllib.request.urlopen

    def stop(self) -> None:
        self._stop.set()

    def run(self) -> None:
        while not self._stop.is_set():
            self.poll_once()
            self._stop.wait(self._next_delay())

    def _next_delay(self) -> float:
        base = self._interval if self._failures == 0 else min(self._interval * (2 ** self._failures), _MAX_BACKOFF)
        return base * random.uniform(0.9, 1.1)  # jitter: fleets don't poll in lockstep

    def poll_once(self) -> None:
        # Auto-instrumentation (e.g. ADOT's urllib instrumentation) must not
        # turn Bindle's own settings requests into traces.
        token = _suppress_instrumentation()
        try:
            self._poll()
        finally:
            if token is not None:
                _otel_context.detach(token)

    def _poll(self) -> None:
        try:
            req = urllib.request.Request(self._url, headers={
                "Authorization": f"Bearer {self._key}", "Accept": "application/json", "User-Agent": self._ua,
                **({"If-None-Match": self._etag} if self._etag else {}),
            })
            try:
                with self._open(req, timeout=self._timeout) as resp:
                    body = resp.read(_MAX_BODY + 1)
                    etag = resp.headers.get("ETag")
            except urllib.error.HTTPError as e:
                if e.code == 304:
                    self._failures = 0
                    return
                raise
            if len(body) > _MAX_BODY:
                raise ValueError("config response too large")
            settings = parse(body)
            if settings is None:
                raise ValueError("unusable config response")
            self._etag, self._interval, self._failures = etag, settings.poll_interval, 0
            self._on_update(settings)
        except Exception as exc:  # noqa: BLE001 - never break the host app
            self._failures = min(self._failures + 1, 10)
            log.debug("bindle: remote config unavailable: %s", exc)
