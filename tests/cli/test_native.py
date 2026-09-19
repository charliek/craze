"""Plan 018 C11: `craze --provider native` from the outside.

The native provider is hidden (plan 018 §3.4): never listed, never persisted,
never indexed. These tests drive the real `craze` binary against a loopback
SSE fixture standing in for a real OpenAI-compatible provider (sse_fixture.py)
and check the CLI-visible contract: text streams and the turn ends end_turn,
`--help` and an unknown-provider error never say "native", a run never
touches config.toml's provider or creates sessions.jsonl, and a canary key
never reaches stdout or stderr -- including when the provider answers 401 or
500 with the key echoed back, the realistic leak path (plan 018 §3.5).
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

from conftest import without_seq
from sse_fixture import CANARY, UNUSED_ENV_KEY, SSEFixture, write_native_config


@pytest.fixture
def fixture_server():
    server = SSEFixture().start()
    try:
        yield server
    finally:
        server.close()


def parse_events(stdout: str) -> list[dict]:
    events = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        events.append(json.loads(line))
    return events


def joined(events: list[dict], event_type: str) -> str:
    return "".join(e.get("text", "") for e in events if e.get("type") == event_type)


def run_native(
    craze_bin: Path,
    craze_home: Path,
    workspace: Path,
    *args: str,
    timeout: float = 10,
    json_mode: bool = True,
) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["CRAZE_HOME"] = str(craze_home)
    env.pop("CRAZE_PROVIDER", None)
    env.pop("CRAZE_AGENT_BIN", None)
    # Never inherited: this is the one variable providers.toml names in
    # env_keys, and a developer or CI runner that happened to export it would
    # make the test pass for the wrong reason (the env var's key, not the
    # inline one the fixture actually expects).
    env.pop(UNUSED_ENV_KEY, None)
    cmd = [str(craze_bin), "prompt"]
    if json_mode:
        cmd.append("--json")
    cmd += ["--provider", "native", "--workspace", str(workspace), *args]
    return subprocess.run(
        cmd,
        check=False,
        capture_output=True,
        text=True,
        env=env,
        timeout=timeout,
    )


def test_native_prompt_streams_text_and_ends_end_turn(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(
        text_parts=["hello ", "from native fixture"],
        reasoning_parts=["thinking it over"],
    )
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi")
    assert proc.returncode == 0, proc.stderr

    events = parse_events(proc.stdout)
    assert joined(events, "text") == "hello from native fixture"
    assert joined(events, "thought") == "thinking it over"
    terminals = [e for e in events if e.get("type") in ("done", "error")]
    assert without_seq(events, terminals) == [{"type": "done", "stopReason": "end_turn"}], events
    assert CANARY not in proc.stdout
    assert CANARY not in proc.stderr


def test_native_prompt_model_flag_resolves_alias(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(text_parts=["picked by alias"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url, alias="fixture-model")

    proc = run_native(craze_bin, craze_home, tmp_path, "--model", "fixture-model", "hi")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "picked by alias"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}


def test_native_absent_from_help_and_unknown_provider_error(
    craze_bin: Path, tmp_path: Path
) -> None:
    help_proc = subprocess.run(
        [str(craze_bin), "--help"], capture_output=True, text=True, timeout=5
    )
    assert help_proc.returncode == 0, help_proc.stderr
    assert "native" not in help_proc.stdout.lower(), help_proc.stdout

    unknown_proc = subprocess.run(
        [
            str(craze_bin),
            "prompt",
            "--provider",
            "nope",
            "--workspace",
            str(tmp_path),
            "hi",
        ],
        capture_output=True,
        text=True,
        timeout=5,
    )
    assert unknown_proc.returncode == 2
    assert "unknown provider" in unknown_proc.stderr
    assert "native" not in unknown_proc.stderr.lower(), unknown_proc.stderr
    assert unknown_proc.stdout == ""


def test_native_run_leaves_config_and_sessions_alone(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(text_parts=["ok"])
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir()
    config_path = craze_home / "config.toml"
    before = 'provider = "grok"\ntheme = "tokyo-night"\n'
    config_path.write_text(before, encoding="utf-8")
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi")
    assert proc.returncode == 0, proc.stderr

    assert config_path.read_text(encoding="utf-8") == before
    assert not (craze_home / "sessions.jsonl").exists()


@pytest.mark.parametrize("mode", ["ok", "unauthorized", "servererror"])
def test_native_canary_never_leaks(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture, mode: str
) -> None:
    """A canary key placed in native/providers.toml appears nowhere in stdout
    or stderr, whether the fixture answers 200, 401, or 500 -- the last two
    with the Authorization header echoed straight into the error body
    (package llm's scrub.go is what has to catch that).
    """
    if mode == "ok":
        fixture_server.set_ok(text_parts=["clean turn"])
    elif mode == "unauthorized":
        fixture_server.set_unauthorized()
    else:
        fixture_server.set_server_error()

    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi", timeout=15)

    # Sanity: the wire really did carry the canary, so its absence below is
    # the scrubber's doing and not an empty response no one looked at.
    assert fixture_server.requests, "the fixture server saw no requests"
    assert any(CANARY in r.authorization for r in fixture_server.requests), fixture_server.requests

    assert CANARY not in proc.stdout, proc.stdout
    assert CANARY not in proc.stderr, proc.stderr

    events = parse_events(proc.stdout)
    if mode == "ok":
        assert proc.returncode == 0, proc.stderr
        assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}
    else:
        assert proc.returncode == 1, proc.stdout + proc.stderr
        errors = [e for e in events if e.get("type") == "error"]
        assert errors, events
        assert CANARY not in errors[0]["message"]
        if mode == "servererror":
            # 401 gets the adapter's own fixed phrasing (phraseTurnError's
            # ErrAuth branch), which never repeats the provider's message at
            # all; 500 falls through to the generic branch, which does
            # include it -- scrubbed, in place of the echoed Authorization
            # header -- so this is where the scrub is actually exercised.
            assert "[redacted]" in errors[0]["message"], errors[0]
        assert not any(e.get("type") == "done" for e in events), events


def test_native_agent_bin_is_a_usage_error(craze_bin: Path, tmp_path: Path) -> None:
    craze_home = tmp_path / "craze-home"
    proc = run_native(
        craze_bin, craze_home, tmp_path, "--agent-bin", "/bin/true", "hi", json_mode=False
    )
    assert proc.returncode == 2, proc.stderr
    assert "--agent-bin" in proc.stderr and "native" in proc.stderr, proc.stderr
    assert proc.stdout == ""


@pytest.mark.parametrize("flag", ["--ask", "--plan"])
def test_native_ask_or_plan_is_a_usage_error(
    craze_bin: Path, tmp_path: Path, flag: str
) -> None:
    craze_home = tmp_path / "craze-home"
    proc = run_native(craze_bin, craze_home, tmp_path, flag, "hi", json_mode=False)
    assert proc.returncode == 2, proc.stderr
    assert "native" in proc.stderr, proc.stderr
    assert proc.stdout == ""
