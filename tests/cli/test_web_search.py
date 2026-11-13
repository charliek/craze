"""Plan 051 C4: the native web tools from the outside, against a fake MCP
endpoint (exa_fixture.py) and the usual scripted model (sse_fixture.py).

Two cases. With `[websearch] provider = "exa"` in config.toml a headless
`craze prompt` is offered `websearch` and `webfetch` at their places in the
tool list, a scripted search and fetch round-trip through the fake, and their
results go back to the model. With no config at all the same headless run
offers exactly the eleven tools it always did and the fake sees nothing: a
session that cannot ask a person is never offered the tools undecided
(plan 051 §C).
"""

from __future__ import annotations

from pathlib import Path

import pytest

from exa_fixture import ExaFixture
from sse_fixture import SSEFixture, answer, call_step, write_native_config
from test_native import (
    fixture_server,  # noqa: F401 (a fixture, used by name)
    joined,
    last_tools,
    parse_events,
    run_native,
    tool_results,
    tool_workspace,
)

# The opencode profile with the web tools, for a session with no background
# jobs (a headless parent), in the registry's order.
WITH_WEB = [
    "bash", "read", "glob", "grep", "edit", "write",
    "agent", "agent_output", "webfetch", "todo_write", "websearch",
    "ask_user_question", "exit_plan_mode",
]  # fmt: skip
WITHOUT_WEB = [t for t in WITH_WEB if t not in ("webfetch", "websearch")]


@pytest.fixture
def exa():
    server = ExaFixture().start()
    try:
        yield server
    finally:
        server.close()


def test_configured_web_search_is_offered_and_round_trips(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    (craze_home / "config.toml").write_text('[websearch]\nprovider = "exa"\n', encoding="utf-8")
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    fixture_server.set_script(
        [
            call_step("websearch", {"query": "go 1.27 release notes"}),
            call_step("webfetch", {"url": "https://go.dev/doc/devel/release"}),
            answer("searched and fetched"),
        ]
    )

    proc = run_native(craze_bin, craze_home, ws, "look it up", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "searched and fetched"

    # The first request offers the web tools at their places in the order.
    assert fixture_server.requests[0].tool_names == WITH_WEB, fixture_server.requests[0].tool_names

    # The fake saw both calls, with the model's arguments.
    assert exa.tools_called() == ["web_search_exa", "web_fetch_exa"], exa.tools_called()
    search, fetch = exa.calls
    assert search.arguments.get("query") == "go 1.27 release notes", search.arguments
    assert "https://go.dev/doc/devel/release" in str(fetch.arguments), fetch.arguments

    # The rows completed, and the replayed text went back to the model.
    rows = last_tools(events)
    assert [r["name"] for r in rows] == ["websearch", "webfetch"], rows
    assert [r["status"] for r in rows] == ["completed", "completed"], rows
    assert len(fixture_server.requests) == 3, len(fixture_server.requests)
    results = tool_results(fixture_server.requests[-1])
    assert set(results) == {"websearch", "webfetch"}, sorted(results)
    assert "Go 1.27 Release Notes" in results["websearch"], results["websearch"][:300]
    assert "Release History" in results["webfetch"], results["webfetch"][:300]


def test_a_failing_endpoint_does_not_round_trip(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The control for the round trip above: the same run against an endpoint
    that answers an error puts the failure, not the replayed page, in the tool
    result -- so the assertions there are about the fake's reply."""
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    (craze_home / "config.toml").write_text('[websearch]\nprovider = "exa"\n', encoding="utf-8")
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    exa.fail = True
    fixture_server.set_script([call_step("websearch", {"query": "anything"}), answer("done")])

    proc = run_native(craze_bin, craze_home, ws, "look it up", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    result = tool_results(fixture_server.requests[-1])["websearch"]
    assert "Go 1.27 Release Notes" not in result, result[:300]
    assert result.startswith("Unable to search the web"), result[:300]


def test_undecided_headless_session_is_offered_no_web_tools(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    fixture_server.set_script([answer("no web here")])

    proc = run_native(craze_bin, craze_home, ws, "hello", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert fixture_server.requests[0].tool_names == WITHOUT_WEB, fixture_server.requests[0].tool_names
    assert exa.calls == [], exa.calls
