"""First-pass redaction that runs inside the customer's process, before any
data leaves it. Mirrors the server-side rules in backend/internal/redact."""

from __future__ import annotations

import logging
import re
from typing import Any, Callable, List, Optional, Pattern, Tuple


def _luhn_ok(s: str) -> bool:
    digits = [int(c) for c in s if c.isdigit()]
    if not 13 <= len(digits) <= 19:
        return False
    total = 0
    for i, d in enumerate(reversed(digits)):
        if i % 2 == 1:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


_Rule = Tuple[str, Pattern[str], Optional[Callable[[str], bool]]]

_DEFAULT_RULES: List[_Rule] = [
    ("PRIVATE_KEY", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----"), None),
    ("SECRET", re.compile(r"\bAKIA[0-9A-Z]{16}\b"), None),
    ("SECRET", re.compile(r"\bsk-[A-Za-z0-9_\-]{20,}"), None),
    ("SECRET", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b"), None),
    ("SECRET", re.compile(r"\bxox[baprs]-[A-Za-z0-9\-]{10,}"), None),
    ("SECRET", re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{20,}=*"), None),
    ("CARD", re.compile(r"\b(?:\d[ \-]?){12,18}\d\b"), _luhn_ok),
    ("SSN", re.compile(r"\b\d{3}-\d{2}-\d{4}\b"), None),
    ("EMAIL", re.compile(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}"), None),
    ("PHONE", re.compile(r"(?:\+?1[\-. ]?)?\(?\b\d{3}\)?[\-. ]?\d{3}[\-. ]?\d{4}\b"), None),
]


class Redactor:
    """Replaces sensitive substrings with ``[REDACTED:<LABEL>]``."""

    def __init__(self, rules: Optional[List[_Rule]] = None, extra_patterns: Optional[dict] = None):
        self._rules: List[_Rule] = list(rules if rules is not None else _DEFAULT_RULES)
        for label, pattern in (extra_patterns or {}).items():
            try:
                self._rules.append((str(label).upper(), re.compile(pattern), None))
            except (re.error, TypeError) as exc:
                # A bad pattern must not disable tracing; ingest redacts again.
                logging.getLogger("bindle").warning("bindle: skipping invalid redaction pattern %r: %s", label, exc)

    def apply(self, value: str) -> str:
        if not value:
            return value
        for label, pattern, check in self._rules:
            marker = f"[REDACTED:{label}]"
            if check is None:
                value = pattern.sub(marker, value)
            else:
                value = pattern.sub(lambda m: marker if check(m.group(0)) else m.group(0), value)
        return value

    def apply_value(self, value: Any) -> Any:
        """Redact an OpenTelemetry attribute value (str or sequence of str)."""
        if isinstance(value, str):
            return self.apply(value)
        if isinstance(value, (list, tuple)):
            return type(value)(self.apply(v) if isinstance(v, str) else v for v in value)
        return value

    def apply_attributes(self, attrs: Optional[dict]) -> dict:
        if not attrs:
            return {}
        return {k: self.apply_value(v) for k, v in attrs.items()}
