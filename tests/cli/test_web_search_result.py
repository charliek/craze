"""Plan 051 C7: the result's `webSearch` from the outside, against the fake MCP
endpoint (exa_fixture.py) and the scripted model (sse_fixture.py).

`craze new --wait --json --web-search` with a scripted search and fetch reports
`offered: true`, the counts and the URLs of the successful results (the search's
result URLs in order, the fetch's requested URL once); the plain footer says
`web: 1 search, 1 fetch`. `--no-web-search` reports `offered: false` and no
counts. A grok session (the fake ACP agent) has no `webSearch` key at all.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from conftest import TEST_HOST_IDLE_EXIT
from exa_fixture import ExaFixture
from sse_fixture import SSEFixture, answer, call_step
from test_native import fixture_server  # noqa: F401 (a fixture, used by name)
from test_waiting import _craze, _hosts_gone, _stop
from test_web_search_flags import _web_home

# The captured search reply's result URLs, in its order
# (internal/harness/websearch/testdata/search.body).
SEARCH_URLS = [
    "https://go.dev/doc/go1.27",
    "https://go.dev/blog/go1.27",
    "https://go.dev/doc/devel/release",
    "https://go.dev/dl/",
    "https://github.com/golang/go/tags",
]


@pytest.fixture
def exa():
    server = ExaFixture().start()
    try:
        yield server
    finally:
        server.close()


def test_new_wait_web_search_reports_its_calls(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A search and a fetch: offered, one of each, the search's URLs in order
    and the fetched page's URL listed once (it is one of them); the same turn's
    plain result says so in its footer, and no URL."""
    _web_home(fixture_server)
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    work = tmp_path / "proj"
    work.mkdir()
    steps = [
        call_step("websearch", {"query": "go 1.27 release notes"}),
        call_step("webfetch", {"url": "https://go.dev/doc/devel/release"}),
        answer("searched and fetched"),
    ]
    fixture_server.set_script(steps + steps)

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--web-search", "-C", str(work), "look it up")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    assert res["status"] == "completed" and res["message"] == "searched and fetched", res
    assert exa.tools_called() == ["web_search_exa", "web_fetch_exa"], exa.tools_called()
    assert res["webSearch"] == {"offered": True, "searches": 1, "fetches": 1, "urls": SEARCH_URLS}, res["webSearch"]
    assert list(res)[-2:] == ["webSearch", "attach"], list(res)

    plain = _craze(craze_bin, "send", res["short"], "--wait", "and again")
    assert plain.returncode == 0, plain
    text = plain.stdout.decode()
    assert "\nweb: 1 search, 1 fetch\n" in text, text
    assert "https://" not in text.split("\n---\n", 1)[1], text
    _stop(craze_bin, res["short"])
    _hosts_gone()


def test_no_web_search_reports_offered_false(
    craze_bin: Path,
    tmp_path: Path,
    fixture_server: SSEFixture,  # noqa: F811
    exa: ExaFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """`--no-web-search`: offered false, always written, and nothing else."""
    _web_home(fixture_server, '\n[websearch]\nprovider = "exa"\n')
    monkeypatch.setenv("CRAZE_TEST_EXA_MCP", exa.url)
    work = tmp_path / "proj"
    work.mkdir()
    fixture_server.set_script([answer("no web")])
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--no-web-search", "-C", str(work), "hello")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    assert res["webSearch"] == {"offered": False}, res
    assert exa.calls == [], exa.calls
    _stop(craze_bin, res["short"])
    _hosts_gone()


def test_a_grok_session_has_no_web_search_key(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """An ACP provider's result never carries webSearch: its host's info
    document has none."""
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\nhost_idle_exit_unattended = "{TEST_HOST_IDLE_EXIT}"\n\n[agents]\ngrok = "{fake_agent_bin}"\n',
        encoding="utf-8",
    )
    work = tmp_path / "proj"
    work.mkdir()
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "grok", "-C", str(work), "hello", script="grok-echo")
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    assert res["status"] == "completed" and res["provider"] == "grok", res
    assert "webSearch" not in res, res
    _stop(craze_bin, res["short"])
    _hosts_gone()
