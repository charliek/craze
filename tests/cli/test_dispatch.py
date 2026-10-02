"""Starting sessions from the list (plan 030 C15-C16, §3.13-§3.14, AC10-AC11):
the list's input starts a new session in any directory, in the background or
in place, and `/provider` and `/model` choose what the next one runs.

Real craze in ptys over the fake agent, as test_sessions.py drives it: one HOME
(the registry, the index, the catalog cache and the config under it), each
session in a workspace of its own, every craze the ordinary, detached one. What
a case reads is the terminal as it stands (Screen) and the files a host leaves
-- its registry entry, its argv, its index row, the catalog cache. Every wait
has a bound of its own; a step that spawns a host gets SPAWN, the rest WAIT.
conftest's host_cleanup stops every host a case leaves, dispatched ones among
them, and fails the case if one will not go.
"""

from __future__ import annotations

import json
import time
from collections.abc import Callable
from pathlib import Path

import pytest
from conftest import _argv, seed_host_idle_exit
from sse_fixture import SSEFixture, write_native_config
from test_detach import _entries
from test_sessions import (
    DOWN,
    ENTER,
    WAIT,
    Screen,
    _entry,
    _group,
    _hint,
    _index_row,
    _open_list,
    _prompt,
    _wait_screen,
)
from test_tui import PTYCraze

# A step that spawns a host -- craze serve, then its agent -- and waits for it
# to answer: under a starved CPU a spawn alone can take several seconds.
SPAWN = 30.0


@pytest.fixture(autouse=True)
def _detached(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """The ordinary, detached craze (conftest's opt-out lifted), with the
    suite's short idle exit seeded in the shared HOME."""
    monkeypatch.delenv("CRAZE_DETACH", raising=False)
    seed_host_idle_exit(tmp_path)


def _tab(craze_bin: Path, fake_agent_bin: Path, home: Path, workspace: Path) -> PTYCraze:
    """craze over the fake agent in workspace, HOME home: one terminal tab.
    Every agent it starts -- its own session's, and those of the hosts its
    list spawns, which inherit its environment -- names its session after the
    directory it runs in (the fake's `{dir}`): the index keys a row by provider
    and that id, and two sessions under one id would be one row."""
    workspace.mkdir(exist_ok=True)
    env = {"HOME": str(home), "CRAZE_FAKE_SESSION_ID": "fake-{dir}"}
    return PTYCraze(craze_bin, fake_agent_bin, workspace, script="echo", env_extra=env)


def _input(rows: list[str]) -> str:
    """The list's input line: the third row from the bottom."""
    return rows[-3]


def _rule(rows: list[str]) -> str:
    """The rule over the input, naming where a new session would run."""
    return rows[-4]


def _type(tui: PTYCraze, screen: Screen, text: str, shown: Callable[[list[str]], bool] | None = None) -> list[str]:
    """Type text into the list's input and wait -- a step's own bound -- for
    the input to show it (and for shown, when given, to hold)."""
    tui.write(text.encode())
    return _wait_screen(
        screen,
        f"the input to hold {text!r}",
        lambda rows: _input(rows).startswith("❯ " + text.rstrip()) and (shown is None or shown(rows)),
    )


def _wait_hint(screen: Screen, what: str, text: str, timeout: float = WAIT) -> list[str]:
    return _wait_screen(screen, what, lambda rows: text in _hint(rows), timeout=timeout)


def _hosts_in(home: Path, workspace: Path) -> list[dict]:
    return [e for e in _entries(home) if Path(e.get("workspace", "")).resolve() == workspace.resolve()]


def _host_logs(home: Path) -> list[Path]:
    """Every host log: each spawn leaves one, whether or not its host came
    up. The hub's log beside them (`hub-<ns>.log`) is not a host's: opening
    the list starts the hub (plan 032 §3.13)."""
    logs = (home / ".cache" / "craze" / "host-logs").glob("*.log")
    return sorted(p for p in logs if not p.name.startswith("hub-"))


def test_a_prompt_starts_a_session_in_another_workspace(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """AC10: `@<another workspace>` and a prompt, enter: a new host is spawned
    in that workspace and given the prompt, in the background -- the list
    stays up, its hint line says where it started, the new session's row
    appears with its reply, and its index row is in that workspace. The
    selector token is not part of the prompt the agent reads."""
    home = tmp_path
    bravo = home / "bravo"
    bravo.mkdir()
    with _tab(craze_bin, fake_agent_bin, home, home / "alpha") as a:
        a.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        sa = Screen(a)
        _open_list(a, sa, "alpha's own row", lambda rows: _group(rows, "idle") is not None)

        rows = _type(a, sa, "@~/bravo tidy the changelog")
        assert "new session → ~/bravo · cursor" in _rule(rows), sa.dump()
        a.write(ENTER)
        _wait_hint(sa, "the dispatch's outcome", "started in ~/bravo", timeout=SPAWN)
        entry = _entry(home, bravo, timeout=SPAWN)
        rows = _wait_screen(
            sa,
            "the new session's row, idle with its reply",
            lambda rows: any(
                "tidy the changelog" in r and "bravo" in r and "echo: tidy" in r for r in _group(rows, "idle") or []
            ),
        )
        assert _input(rows).startswith("❯ type a prompt"), f"the input was not cleared:\n{sa.dump()}"
        row = _index_row(home, entry["crazeSessionId"])
        assert Path(row["cwd"]).resolve() == bravo.resolve(), row
        assert row["title"] == "tidy the changelog", row
        assert a.proc.poll() is None


def test_an_at_directory_alone_opens_an_unstarted_session(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """AC10: a picked `@dir` alone, enter: an unstarted session opens in place
    -- the band names it new, in that directory -- and nothing is spawned
    until its first prompt, which spawns its host there, adopts it, and is
    answered in this terminal."""
    home = tmp_path
    bravo = home / "bravo"
    bravo.mkdir()
    with _tab(craze_bin, fake_agent_bin, home, home / "alpha") as a:
        a.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        sa = Screen(a)
        _open_list(a, sa, "alpha's own row", lambda rows: _group(rows, "idle") is not None)

        # The browse popup lists ~'s folders; enter picks bravo and binds it.
        _type(a, sa, "@~/bravo", lambda rows: any(r.startswith("❯ bravo/") for r in rows[:-4]))
        a.write(ENTER)
        # Picked, the token is bound to bravo: the popup closes and the rule
        # names it.
        _wait_screen(
            sa,
            "the pick written and bound",
            lambda rows: _input(rows).startswith("❯ @~/bravo") and "new session → ~/bravo · cursor" in _rule(rows),
        )
        # One spawn so far -- this terminal's own session's -- and one log.
        logs = _host_logs(home)
        assert len(logs) == 1, logs
        a.write(ENTER)
        # The band drawn: the TUI has handled the enter, and nothing is
        # spawned for the session in its handling.
        _wait_screen(
            sa, "the unstarted session in place", lambda rows: rows[0].startswith("─ new session · cursor · ~/bravo ")
        )
        assert not _hosts_in(home, bravo), _entries(home)

        mark = a.mark()
        a.write(b"first words" + ENTER)
        a.wait_contains_since("echo: first words", mark, timeout=SPAWN)
        entry = _entry(home, bravo, timeout=WAIT)
        _wait_screen(sa, "the session up in bravo", lambda rows: "· cursor · bravo " in rows[0])
        row = _index_row(home, entry["crazeSessionId"])
        assert Path(row["cwd"]).resolve() == bravo.resolve(), row
        # And nothing else was spawned for it. This is the end-to-end
        # corroboration, not the proof: nothing here joins a spawn the
        # opening might have scheduled late, so a delayed one could still
        # slip past it. The deterministic proof that opening spawns nothing is
        # the Go test TestAnUnstartedSessionOpensInPlaceWithNothingSpawned
        # (internal/tui/dispatch_test.go): no Spawn or Open asked in the
        # enter's handling, nor by any command it handed back, each run to
        # its end. Here: a host the opening had spawned by the time the first
        # prompt's came up would have left a log of its own and, running, a
        # second registry entry in bravo.
        assert len(_host_logs(home)) == 2, _host_logs(home)
        assert [e["hostId"] for e in _hosts_in(home, bravo)] == [entry["hostId"]], _entries(home)


def test_provider_and_model_change_the_next_dispatch(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """AC11: the host of this terminal's cursor session records the catalog
    its agent installed (the catalog cache); `/model` lists it, its age in
    the title, and a model chosen from it is the next dispatch's --model --
    the catalog's own `default` model included (--model=default), which is
    not the agent's own default listed above it (no --model at all).
    `/effort high` and `/fast on` (plan 032 C17) are that dispatch's --effort
    and --fast, and `default` clears both for the next one.
    `/provider native` resets the model to native's default from its model
    table, and the next dispatch runs native on it -- no agent binary."""
    home = tmp_path
    bravo, charlie, delta, echo = home / "bravo", home / "charlie", home / "delta", home / "echo"
    for d in (bravo, charlie, delta, echo):
        d.mkdir()
    fixture = SSEFixture().start()
    try:
        write_native_config(home / ".craze" / "native", fixture.base_url)
        with _tab(craze_bin, fake_agent_bin, home, home / "alpha") as a:
            a.wait_contains("cursor", timeout=WAIT)
            _prompt(a, "alpha one")
            catalog = home / ".cache" / "craze" / "catalogs" / "cursor.json"
            deadline = time.monotonic() + WAIT
            while not catalog.exists() and time.monotonic() < deadline:
                time.sleep(0.05)
            assert catalog.exists(), f"no catalog cache at {catalog} after {WAIT}s"
            cached = json.loads(catalog.read_text(encoding="utf-8"))
            assert cached["provider"] == "cursor" and [m["id"] for m in cached["models"]] == ["default", "composer"], (
                cached
            )

            sa = Screen(a)
            _open_list(a, sa, "alpha's own row", lambda rows: _group(rows, "idle") is not None)
            rows = _type(
                a,
                sa,
                "/model comp",
                lambda rows: (
                    any("cursor models for new sessions · last seen" in r for r in rows)
                    and any(r.startswith("❯ Composer") for r in rows[:-4])
                ),
            )
            a.write(ENTER)
            rows = _wait_hint(sa, "the model chosen", "new sessions use cursor · Composer")
            assert "· cursor · Composer ─" in _rule(rows), sa.dump()

            # /effort and /fast, each revealed by its prefix and chosen from
            # its values (C17): the fake agent offers neither, so its host
            # notes them unmatched and starts the session anyway.
            _type(a, sa, "/effort high", lambda rows: any(r.startswith("❯ high") for r in rows[:-4]))
            a.write(ENTER)
            _wait_hint(sa, "the effort chosen", "new sessions use cursor · Composer · high")
            _type(a, sa, "/fast on", lambda rows: any(r.startswith("❯ on") for r in rows[:-4]))
            a.write(ENTER)
            rows = _wait_hint(sa, "fast mode chosen", "new sessions use cursor · Composer · high · fast")
            assert "· cursor · Composer · high · fast ─" in _rule(rows), sa.dump()

            _type(a, sa, "@~/bravo on composer")
            a.write(ENTER)
            _wait_hint(sa, "the first dispatch's outcome", "started in ~/bravo", timeout=SPAWN)
            argv = _argv(_entry(home, bravo, timeout=SPAWN)["pid"])
            assert "--provider=cursor" in argv and "--model=composer" in argv, argv
            assert "--effort=high" in argv and "--fast" in argv, argv

            for line in ("/effort default", "/fast default"):
                _type(a, sa, line, lambda rows: any(r.startswith("❯ default") for r in rows[:-4]))
                a.write(ENTER)
                _wait_screen(sa, f"{line} applied", lambda rows: _input(rows).startswith("❯ type a prompt"))
            rows = _wait_hint(sa, "both cleared", "new sessions use cursor · Composer")
            assert "· cursor · Composer ─" in _rule(rows), sa.dump()

            # The fake agent's catalog has a model whose id is `default`: it
            # is listed as itself, under the agent's own default (C15r).
            _type(
                a,
                sa,
                "/model defa",
                lambda rows: (
                    any(r.startswith("❯ default") and "the agent's own choice" in r for r in rows[:-4])
                    and any(r.startswith("  Default") for r in rows[:-4])
                ),
            )
            a.write(DOWN)
            _wait_screen(
                sa, "the catalog's default highlighted", lambda rows: any(r.startswith("❯ Default") for r in rows[:-4])
            )
            a.write(ENTER)
            _wait_hint(sa, "the catalog's default chosen", "new sessions use cursor · Default")
            _type(a, sa, "@~/delta on the catalog's default")
            a.write(ENTER)
            _wait_hint(sa, "the catalog's default dispatch", "started in ~/delta", timeout=SPAWN)
            argv = _argv(_entry(home, delta, timeout=SPAWN)["pid"])
            assert "--model=default" in argv, argv
            assert not any(arg.startswith(("--effort", "--fast", "--no-fast")) for arg in argv), argv

            _type(
                a,
                sa,
                "/model defa",
                lambda rows: any(r.startswith("❯ default") and "the agent's own choice" in r for r in rows[:-4]),
            )
            a.write(ENTER)
            rows = _wait_hint(sa, "the agent's own default chosen", "new sessions use cursor · default")
            assert "· cursor · default ─" in _rule(rows), sa.dump()
            _type(a, sa, "@~/echo on the agent's own")
            a.write(ENTER)
            _wait_hint(sa, "the agent's own default dispatch", "started in ~/echo", timeout=SPAWN)
            argv = _argv(_entry(home, echo, timeout=SPAWN)["pid"])
            assert not any(arg.startswith("--model") for arg in argv), argv

            _type(a, sa, "/provider native", lambda rows: any(r.startswith("❯ native") for r in rows[:-4]))
            a.write(ENTER)
            rows = _wait_hint(sa, "native chosen, on its default", "new sessions use native · Fixture Model")
            assert "· native · Fixture Model ─" in _rule(rows), sa.dump()

            _type(a, sa, "@~/charlie on native")
            a.write(ENTER)
            _wait_hint(sa, "the second dispatch's outcome", "started in ~/charlie", timeout=SPAWN)
            argv = _argv(_entry(home, charlie, timeout=SPAWN)["pid"])
            assert "--provider=native" in argv and "--model=fixture-model" in argv, argv
            assert not any(arg.startswith("--agent-bin") for arg in argv), argv
            _wait_screen(
                sa,
                "the native session's row",
                lambda rows: any("on native" in r and "native" in r and "charlie" in r for r in rows),
                timeout=SPAWN,
            )
    finally:
        fixture.close()
