import os

import pytest


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch):
    """Tests must not depend on the developer's shell (e.g. deploy/.env)."""
    for name in list(os.environ):
        if name.startswith(("BINDLE_", "OTEL_")):
            monkeypatch.delenv(name, raising=False)
