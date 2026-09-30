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


def _no_host_in(home: Path, workspace: Path) -> bool:
    return all(Path(e.get("workspace", "")).resolve() != workspace.resolve() for e in _entries(home))


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
        a.write(ENTER)
        _wait_screen(
            sa, "the unstarted session in place", lambda rows: rows[0].startswith("─ new session · cursor · ~/bravo ")
        )
        # Nothing spawned for it: a moment is far past a spawn's first write.
        time.sleep(0.5)
        assert _no_host_in(home, bravo), _entries(home)

        mark = a.mark()
        a.write(b"first words" + ENTER)
        a.wait_contains_since("echo: first words", mark, timeout=SPAWN)
        entry = _entry(home, bravo, timeout=WAIT)
        _wait_screen(sa, "the session up in bravo", lambda rows: "· cursor · bravo " in rows[0])
        row = _index_row(home, entry["crazeSessionId"])
        assert Path(row["cwd"]).resolve() == bravo.resolve(), row


def test_provider_and_model_change_the_next_dispatch(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """AC11: the host of this terminal's cursor session records the catalog
    its agent installed (the catalog cache); `/model` lists it, its age in
    the title, and a model chosen from it is the next dispatch's --model.
    `/provider native` resets the model to native's default from its model
    table, and the next dispatch runs native on it -- no agent binary."""
    home = tmp_path
    bravo, charlie = home / "bravo", home / "charlie"
    bravo.mkdir()
    charlie.mkdir()
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

            _type(a, sa, "@~/bravo on composer")
            a.write(ENTER)
            _wait_hint(sa, "the first dispatch's outcome", "started in ~/bravo", timeout=SPAWN)
            argv = _argv(_entry(home, bravo, timeout=SPAWN)["pid"])
            assert "--provider=cursor" in argv and "--model=composer" in argv, argv

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
