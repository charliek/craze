from __future__ import annotations

import os
from collections.abc import Mapping
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# The config-file variable CRAZE_HOME replaced. craze refuses to run while it
# is set, so an inherited one is scrubbed below. It is spelled in two parts on
# purpose: the repo-walk test in internal/paths keeps the whole name out of
# every file outside that package, so a test still isolating itself with it
# fails CI instead of reaching the developer's real ~/.craze.
REMOVED_CONFIG_ENV = "CRAZE_" + "CONFIG"


def _bin(env_name: str, *parts: str) -> Path:
    override = os.environ.get(env_name)
    if override:
        return Path(override)
    path = ROOT.joinpath(*parts)
    if not path.is_file():
        pytest.fail(f"{path} is missing; run `make build` (or set {env_name})")
    return path


@pytest.fixture(scope="session")
def craze_bin() -> Path:
    return _bin("CRAZE_BIN", "bin", "craze")


@pytest.fixture(scope="session")
def fake_agent_bin() -> Path:
    return _bin("CRAZE_FAKE_AGENT_BIN", "bin", "craze-fake-agent")


@pytest.fixture(autouse=True)
def isolate_run_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Isolate everything a craze run reads out of the environment.

    Every helper here builds its child environment from ``os.environ``, so one
    fixture covers them all. HOME is in it because craze walks the plugin
    caches under HOME at session start: without it a run would find whatever
    the developer happens to have installed. CRAZE_HOME is set explicitly --
    never left to follow HOME -- so a developer's exported one cannot leak in,
    and the config file and session index are ``<tmp>/craze-home/config.toml``
    and ``<tmp>/craze-home/sessions.jsonl``.
    """
    home = tmp_path / "home"
    home.mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("CRAZE_PROVIDER", "")
    monkeypatch.setenv("CRAZE_HOME", str(tmp_path / "craze-home"))
    monkeypatch.delenv(REMOVED_CONFIG_ENV, raising=False)
    # The session journal's opt-out (plan 020 §3.5). An exported one would
    # otherwise turn journaling off under the cases that assert a journal was
    # written -- or, unparseable, print a diagnostic into a run whose stderr a
    # case reads. A case that wants it sets it itself.
    monkeypatch.delenv("CRAZE_JOURNAL", raising=False)
    # craze reports its status to the terminal multiplexer it runs in (herdr,
    # roost) whenever that host's variables say so -- and this suite is often
    # run from inside one. Inherited, they would point a test craze at the
    # developer's own live pane or tab. Every host variable goes; a test that
    # wants a host sets exactly the ones it needs, aimed at its own fake socket.
    for name in host_env_names(os.environ):
        monkeypatch.delenv(name, raising=False)


def host_env_names(env: Mapping[str, str]) -> list[str]:
    """Every herdr and roost variable in env: the hosts craze may report to."""
    return [name for name in env if name.startswith(("HERDR_", "ROOST_"))]
