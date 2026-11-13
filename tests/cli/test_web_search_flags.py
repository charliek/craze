"""Plan 051 C6: `--web-search` and `--no-web-search` from the outside, against
the fake MCP endpoint (exa_fixture.py) and the scripted model (sse_fixture.py).

`craze new --wait` with either flag records the choice on the session's index
row (`webSearch`), and `craze send`'s reload of the stopped session applies it
again; `--no-web-search` beats a config.toml that turns web search on;
`websearch = false` beats `--web-search`, which is refused before the prompt is
read (`web_search_not_offered`), and refuses a reload nobody attends of a
session created with it; `craze prompt` takes the flags too, refusing with its
startup `error` event.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from conftest import TEST_HOST_IDLE_EXIT
from exa_fixture import ExaFixture
from sse_fixture import SSEFixture, answer, call_step, write_native_config
from test_detach import _entries
from test_native import (
    fixture_server,  # noqa: F401 (a fixture, used by name)
    parse_events,
    run_native,
    tool_workspace,
)
from test_waiting import _craze, _home, _hosts_gone, _stop
from test_web_search import WITH_WEB, WITHOUT_WEB


@pytest.fixture
def exa():
    server = ExaFixture().start()
    try:
        yield server
    finally:
        server.close()


def _web_home(fixture: SSEFixture, websearch: str = "") -> Path:
    """The test's CRAZE_HOME for a waiting native session over fixture: the
    suite's idle exit, then websearch's lines (a `websearch` key or table, or
    nothing), and the native model table aimed at the fixture."""
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\nhost_idle_exit_unattended = "{TEST_HOST_IDLE_EXIT}"\n{websearch}',
        encoding="utf-8",
    )
    write_native_config(craze_home / "native", fixture.base_url)
    return craze_home


def _row(craze_home: Path, session: str) -> dict:
    """The session index's newest row of craze session id session."""
    lines = (craze_home / "sessions.jsonl").read_text(encoding="utf-8").splitlines()
    mine = [r for r in (json.loads(line) for line in lines if line.strip()) if r.get("crazeId") == session]
    assert mine, lines
    return max(mine, key=lambda r: r["updatedAt"])


def _offers_web(request) -> bool:  # noqa: ANN001 (sse_fixture's request type)
    names = request.tool_names
    assert ("websearch" in names) == ("webfetch" in names), names
    return "websearch" in names


def test_new_wait_web_search_is_recorded_and_kept_across_a_reload(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """`craze new --wait --web-search` in a home that says nothing offers the
    unattended session the web tools, a search round-trips through the fake,
    the row records `webSearch: on` beside `unattended`, nothing is
    remembered, and `craze send`'s reload of the stopped session offers them
    again."""
    craze_home = _web_home(fixture_server)
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    work = tmp_path / "proj"
    work.mkdir()
    fixture_server.set_script([call_step("websearch", {"query": "go release notes"}), answer("searched"), answer("again")])

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--web-search", "-C", str(work), "look it up")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    assert res["status"] == "completed" and res["message"] == "searched", res
    assert _offers_web(fixture_server.requests[0])
    assert exa.tools_called() == ["web_search_exa"], exa.tools_called()
    row = _row(craze_home, res["session"])
    assert row.get("webSearch") == "on" and "unattended" in row, row
    assert not (craze_home / "native" / "websearch.json").exists()

    _stop(craze_bin, res["short"])
    before = len(fixture_server.requests)
    out = _craze(craze_bin, "send", res["short"], "--wait", "--json", "and again")
    assert out.returncode == 0, out
    assert json.loads(out.stdout)["message"] == "again", out
    assert len(fixture_server.requests) == before + 1, len(fixture_server.requests)
    assert _offers_web(fixture_server.requests[-1]), "the reload dropped the session's --web-search"
    assert _row(craze_home, res["session"]).get("webSearch") == "on"
    _stop(craze_bin, res["short"])
    _hosts_gone()


def test_no_web_search_beats_a_config_that_is_on_on_every_load(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """With `[websearch]` on in config.toml, `--no-web-search` offers no web
    tools, records `off`, and a reload keeps it; the control, a session with
    neither flag in the same home, is offered them and records nothing."""
    craze_home = _web_home(fixture_server, '\n[websearch]\nprovider = "exa"\n')
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    work = tmp_path / "proj"
    work.mkdir()
    fixture_server.set_script([answer("one"), answer("two"), answer("three")])

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--no-web-search", "-C", str(work), "hello")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    assert not _offers_web(fixture_server.requests[0]), "--no-web-search lost to the config"
    assert _row(craze_home, res["session"]).get("webSearch") == "off"
    _stop(craze_bin, res["short"])
    out = _craze(craze_bin, "send", res["short"], "--wait", "--json", "again")
    assert out.returncode == 0, out
    assert not _offers_web(fixture_server.requests[-1]), "the reload dropped the session's --no-web-search"
    _stop(craze_bin, res["short"])

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "-C", str(work), "control")
    assert out.returncode == 0, out
    control = json.loads(out.stdout)
    assert _offers_web(fixture_server.requests[-1]), "the config does not turn web search on: the test proves nothing"
    assert "webSearch" not in _row(craze_home, control["session"])
    _stop(craze_bin, control["short"])
    assert exa.calls == [], exa.calls
    _hosts_gone()


def test_new_wait_web_search_refused_by_an_explicit_off(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
) -> None:
    """`websearch = false` beats `--web-search`: exit 1, kind config, the
    reason with no model, before the prompt is read (the prompt file is still
    there, --remove-prompt-file notwithstanding) and before any host or model
    request."""
    _web_home(fixture_server, "websearch = false\n")
    task = tmp_path / "task.md"
    task.write_text("the task\n", encoding="utf-8")
    out = _craze(
        craze_bin, "new", "--wait", "--json", "--provider", "native", "--web-search",
        "-C", str(tmp_path), "--prompt-file", str(task), "--remove-prompt-file",
    )  # fmt: skip
    assert out.returncode == 1, out
    assert json.loads(out.stdout) == {
        "error": {
            "message": "--web-search cannot be honoured: config.toml turns web search off (websearch = false)",
            "kind": "config",
            "retryable": False,
            "detail": {"reason": "web_search_not_offered"},
        }
    }, out.stdout
    assert out.stderr == b"craze new: --web-search cannot be honoured: config.toml turns web search off (websearch = false)\n", out
    assert task.read_text(encoding="utf-8") == "the task\n"
    assert fixture_server.requests == [] and _entries(_home()) == []


def test_a_reload_nobody_attends_refuses_an_on_the_config_turned_off(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A session created with --web-search, stopped, and then `websearch =
    false` written: `craze send`'s unattended reload fails, exit 1, kind
    config, in the host's words, and leaves no host; with the config back it
    reloads, offered the tools."""
    craze_home = _web_home(fixture_server)
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    work = tmp_path / "proj"
    work.mkdir()
    fixture_server.set_script([answer("one"), answer("two")])
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--web-search", "-C", str(work), "hello")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    _stop(craze_bin, res["short"])

    config = craze_home / "config.toml"
    kept = config.read_text(encoding="utf-8")
    config.write_text(kept + "websearch = false\n", encoding="utf-8")
    before = len(fixture_server.requests)
    out = _craze(craze_bin, "send", res["short"], "--wait", "--json", "again")
    assert out.returncode == 1, out
    err = json.loads(out.stdout)["error"]
    assert err["kind"] == "config" and "detail" not in err, err
    assert err["message"].endswith(
        "this session was created with --web-search, which cannot be honoured: config.toml turns web search off (websearch = false)"
    ), err
    assert len(fixture_server.requests) == before
    _hosts_gone()

    config.write_text(kept, encoding="utf-8")
    out = _craze(craze_bin, "send", res["short"], "--wait", "--json", "again")
    assert out.returncode == 0 and json.loads(out.stdout)["message"] == "two", out
    assert _offers_web(fixture_server.requests[-1])
    _stop(craze_bin, res["short"])
    _hosts_gone()


def test_prompt_web_search_flags(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """`craze prompt --web-search` with nothing in the home is offered the
    tools and a search round-trips, with nothing remembered; refused by
    `websearch = false`, exit 1, one startup `error` event and no model
    request; `--no-web-search` over a config that is on is offered none."""
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    fixture_server.set_script([call_step("websearch", {"query": "anything"}), answer("searched")])
    proc = run_native(craze_bin, craze_home, ws, "--web-search", "look it up", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert fixture_server.requests[0].tool_names == WITH_WEB, fixture_server.requests[0].tool_names
    assert exa.tools_called() == ["web_search_exa"], exa.tools_called()
    assert not (craze_home / "native" / "websearch.json").exists()

    (craze_home / "config.toml").write_text("websearch = false\n", encoding="utf-8")
    before = len(fixture_server.requests)
    proc = run_native(craze_bin, craze_home, ws, "--web-search", "look it up")
    want = "craze prompt: --web-search cannot be honoured: config.toml turns web search off (websearch = false)"
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert parse_events(proc.stdout) == [{"type": "error", "message": want}], proc.stdout
    assert proc.stderr == want + "\n", proc.stderr
    assert len(fixture_server.requests) == before

    (craze_home / "config.toml").write_text('[websearch]\nprovider = "exa"\n', encoding="utf-8")
    fixture_server.set_script([answer("no web")])
    proc = run_native(craze_bin, craze_home, ws, "--no-web-search", "hello")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert fixture_server.requests[-1].tool_names == WITHOUT_WEB, fixture_server.requests[-1].tool_names
