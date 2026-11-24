"""Learned skills: approved SKILL.md files Bindle ships to an agent.

``bindle.init()`` starts a background fetch of the skills shipped to this
agent (and version) from ``GET {endpoint}/v1/skills``. It is time-boxed,
cached on disk with an ETag, and silent on failure: if Bindle is down the
agent keeps the last cached skills, or runs without any. Nothing here ever
raises into the agent.

Use them with progressive disclosure (agentskills.io)::

    for s in bindle.skills.relevant(task_text):   # names and descriptions only
        prompt += bindle.skills.load(s)            # the full instructions; recorded on the run

``load()`` records a ``bindle.skill.used`` span event (path and release) on
the current step, so runs show which skills were loaded and Bindle can
measure each shipped version. The console's kill switches arrive with the
SDK config and take effect within one config poll.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import tempfile
import threading
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Callable, List, Optional, Sequence, Tuple, Union

log = logging.getLogger("bindle")

FETCH_TIMEOUT = 2.0  # seconds: never more on the agent's account
REFRESH = 300.0  # seconds between fetches
IDLE_REFRESH = 3600.0  # when the workspace doesn't ship skills (404)
USED_EVENT = "bindle.skill.used"
_MAX_SKILLS = 100
_MAX_BODY = 64 << 10
_WORD = re.compile(r"[a-z0-9]+")
_STOP = frozenset("""a an and are as at be but by can do for from has have how i if in into is it its me my not of on or our
should so than that the their then there these this to use used uses using was we what when where which while who why will
with you your""".split())


@dataclass(frozen=True)
class Skill:
    """A shipped skill. ``body`` is the full SKILL.md; prefer ``load()``,
    which also records that the run used it."""

    name: str
    description: str
    path: str
    release: int
    sha256: str
    body: str = field(repr=False)
    agent_versions: Tuple[str, ...] = ()


def _parse(doc: dict) -> List[Skill]:
    out: List[Skill] = []
    for s in (doc.get("skills") or [])[:_MAX_SKILLS]:
        try:
            body = str(s["body"])
            if len(body) > _MAX_BODY:
                continue
            out.append(Skill(str(s["name"]), str(s.get("description") or ""), str(s["path"]), int(s["release"]),
                             str(s.get("sha256") or ""), body, tuple(str(v) for v in s.get("agent_versions") or ())))
        except (KeyError, TypeError, ValueError):
            continue
    return out


def _cache_dir() -> str:
    return os.environ.get("BINDLE_CACHE_DIR") or os.path.join(os.path.expanduser("~"), ".cache", "bindle")


class SkillStore:
    """The skills shipped to one agent, refreshed in the background."""

    def __init__(self, endpoint: str, api_key: str, agent: str, version: str = "",
                 killed: Callable[[], Sequence[str]] = lambda: (), user_agent: str = "bindle-sdk-python",
                 opener=None, cache_dir: Optional[str] = None):
        self._url = endpoint.rstrip("/") + "/v1/skills?" + urllib.parse.urlencode({"agent": agent, "version": version})
        self._key, self._agent, self._killed, self._ua = api_key, agent, killed, user_agent
        self._open = opener or urllib.request.urlopen
        digest = hashlib.sha256(f"{endpoint}\x00{agent}\x00{version}".encode()).hexdigest()[:24]
        self._path = os.path.join(cache_dir or _cache_dir(), f"skills-{digest}.json")
        self._lock = threading.Lock()
        self._skills: List[Skill] = []
        self._etag: Optional[str] = None
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._load_cache()

    # -- fetching ------------------------------------------------------------

    def _load_cache(self) -> None:
        try:
            with open(self._path, encoding="utf-8") as f:
                doc = json.load(f)
            with self._lock:
                self._skills, self._etag = _parse(doc), doc.get("etag")
        except Exception:  # noqa: BLE001 - no or unreadable cache: start empty
            pass

    def _save_cache(self, doc: dict) -> None:
        try:
            os.makedirs(os.path.dirname(self._path), mode=0o700, exist_ok=True)
            fd, tmp = tempfile.mkstemp(dir=os.path.dirname(self._path), prefix=".skills-")
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(doc, f)
            os.replace(tmp, self._path)
        except Exception as exc:  # noqa: BLE001
            log.debug("bindle: skills cache not written: %s", exc)

    def refresh(self) -> float:
        """Fetches once; returns how long to wait before the next fetch.
        Errors keep the current skills."""
        req = urllib.request.Request(self._url, headers={"Authorization": f"Bearer {self._key}", "User-Agent": self._ua})
        if self._etag:
            req.add_header("If-None-Match", self._etag)
        try:
            with self._open(req, timeout=FETCH_TIMEOUT) as resp:
                body = resp.read(4 << 20)
                doc = json.loads(body.decode("utf-8"))
                doc["etag"] = resp.headers.get("ETag")
            skills = _parse(doc)
            with self._lock:
                self._skills, self._etag = skills, doc["etag"]
            self._save_cache(doc)
            return REFRESH
        except urllib.error.HTTPError as e:
            if e.code == 304:
                return REFRESH
            if e.code == 404:  # the workspace doesn't ship skills: none, and ask rarely
                with self._lock:
                    self._skills, self._etag = [], None
                self._save_cache({"skills": []})
                return IDLE_REFRESH
            log.debug("bindle: skills fetch: HTTP %s", e.code)
        except Exception as exc:  # noqa: BLE001 - Bindle down: keep the last skills
            log.debug("bindle: skills fetch failed: %s", exc)
        return REFRESH

    def start(self) -> None:
        def loop() -> None:
            wait = self.refresh()
            while not self._stop.wait(wait):
                wait = self.refresh()

        self._thread = threading.Thread(target=loop, name="bindle-skills", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()

    # -- using ---------------------------------------------------------------

    def list(self) -> List[Skill]:
        """Every shipped skill the kill switches allow."""
        try:
            killed = set(self._killed() or ())
        except Exception:  # noqa: BLE001
            killed = set()
        if f"{self._agent}/*" in killed:
            return []
        with self._lock:
            return [s for s in self._skills if f"{self._agent}/{s.path}" not in killed]


_store: Optional[SkillStore] = None


def _set_store(store: Optional[SkillStore]) -> None:
    global _store
    if _store is not None:
        _store.stop()
    _store = store


def _words(text: str) -> set:
    return {w for w in _WORD.findall(text.lower()) if len(w) > 2 and w not in _STOP}


def list() -> List[Skill]:  # noqa: A001 - bindle.skills.list()
    """The skills shipped to this agent (none before init or when off)."""
    try:
        return _store.list() if _store is not None else []
    except Exception:  # noqa: BLE001
        return []


def relevant(task: str, limit: int = 3) -> List[Skill]:
    """Skills whose name and description share words with the task, best
    match first. Only names and descriptions are compared (agentskills.io
    progressive disclosure); call ``load()`` for the ones you use."""
    try:
        want = _words(task or "")
        scored = []
        for s in list():
            n = len(want & _words(s.name.replace("-", " ") + " " + s.description))
            if n:
                scored.append((-n, s.name, s))
        return [s for _, _, s in sorted(scored)[:max(limit, 0)]]
    except Exception:  # noqa: BLE001
        return []


def get(name: str) -> Optional[Skill]:
    """A shipped skill by name (None if not shipped or switched off)."""
    for s in list():
        if s.name == name:
            return s
    return None


def load(skill: Union[Skill, str]) -> str:
    """The skill's full SKILL.md text ("" if it isn't shipped or was
    switched off), recording a ``bindle.skill.used`` event on the current
    step."""
    try:
        s = get(skill) if isinstance(skill, str) else skill
        if s is None or s not in list():
            return ""
        from opentelemetry import trace

        span = trace.get_current_span()
        if span.is_recording():
            span.add_event(USED_EVENT, {"bindle.skill.name": s.name, "bindle.skill.path": s.path,
                                        "bindle.skill.release": s.release})
        return s.body
    except Exception:  # noqa: BLE001
        return ""
