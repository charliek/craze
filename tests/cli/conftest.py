from __future__ import annotations

import os
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


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
    the developer happens to have installed.
    """
    home = tmp_path / "home"
    home.mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("CRAZE_PROVIDER", "")
    monkeypatch.setenv("CRAZE_CONFIG", str(tmp_path / "craze-config.toml"))
