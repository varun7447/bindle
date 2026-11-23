"""Configuration resolution: explicit arguments win, then environment
variables, then safe defaults. No config file is ever required."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import List, Optional, Union

def _env_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, default))
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    v = os.environ.get(name)
    if v is None:
        return default
    return v.strip().lower() not in ("0", "false", "no", "off", "")


@dataclass
class Config:
    api_key: Optional[str]
    endpoint: str  # empty when not configured: the SDK is then a no-op
    agent: Optional[str]
    version: Optional[str]
    environment: Optional[str]
    sample_rate: float
    redact: bool
    capture_content: bool
    instrument: Union[bool, List[str]]
    debug: bool
    extra_redact_patterns: dict = field(default_factory=dict)
    remote_config: bool = True

    @property
    def traces_url(self) -> str:
        return self.endpoint.rstrip("/") + "/v1/traces"

    @property
    def config_url(self) -> str:
        return self.endpoint.rstrip("/") + "/v1/config"


def resolve(
    api_key: Optional[str] = None,
    endpoint: Optional[str] = None,
    agent: Optional[str] = None,
    version: Optional[str] = None,
    environment: Optional[str] = None,
    sample_rate: Optional[float] = None,
    redact: Optional[bool] = None,
    capture_content: Optional[bool] = None,
    instrument: Union[bool, List[str], None] = None,
    debug: Optional[bool] = None,
    extra_redact_patterns: Optional[dict] = None,
    remote_config: Optional[bool] = None,
) -> Config:
    rate = sample_rate if sample_rate is not None else _env_float("BINDLE_SAMPLE_RATE", 1.0)
    rate = min(max(rate, 0.0), 1.0)
    return Config(
        api_key=api_key or os.environ.get("BINDLE_API_KEY"),
        endpoint=(endpoint or os.environ.get("BINDLE_ENDPOINT") or "").strip(),
        agent=agent or os.environ.get("BINDLE_AGENT") or os.environ.get("OTEL_SERVICE_NAME"),
        version=version or os.environ.get("BINDLE_AGENT_VERSION"),
        environment=environment or os.environ.get("BINDLE_ENVIRONMENT"),
        sample_rate=rate,
        redact=redact if redact is not None else _env_bool("BINDLE_REDACT", True),
        capture_content=capture_content if capture_content is not None else _env_bool("BINDLE_CAPTURE_CONTENT", True),
        instrument=instrument if instrument is not None else _env_bool("BINDLE_AUTO_INSTRUMENT", True),
        debug=debug if debug is not None else _env_bool("BINDLE_DEBUG", False),
        extra_redact_patterns=extra_redact_patterns or {},
        remote_config=remote_config if remote_config is not None else _env_bool("BINDLE_REMOTE_CONFIG", True),
    )

