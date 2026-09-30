"""The session list (plan 030 C12, §3.10-§3.12, AC8-AC9): every session of
this user on this machine, one row each, opened with `←` on an empty composer.

Real craze in ptys over the fake agent, every one with the same HOME -- the
registry, the index and the config under it -- and each in a workspace of its
own, as terminal tabs in two projects would be. Every case runs the ordinary,
detached craze (no CRAZE_DETACH=0): the list exists only then. What a case
reads is the terminal as it stands (Screen), not the output's history: the
list redraws its rows in place, so "the row under the idle header" is a fact
about the screen, never about bytes that once went past. Every wait has a
bound of its own; conftest's host_cleanup stops whatever host a case leaves
and fails the case if one will not go.
"""

from __future__ import annotations

import codecs
import json
import os
import re
import signal
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from pathlib import Path

import pytest
from conftest import pid_alive, seed_host_idle_exit
from test_detach import _entries
from test_tui import PTYCraze

WAIT = 10.0

LEFT, UP, DOWN = b"\x1b[D", b"\x1b[A", b"\x1b[B"
ENTER, CTRL_D, CTRL_S, CTRL_X = b"\r", b"\x04", b"\x13", b"\x18"

# The list's state glyphs (§3.10): idle, saved (and ended), unreachable; a
# working row's is the spinner's current frame.
IDLE, SAVED, UNREACHABLE = "○", "·", "?"
SPINNER = frozenset("✳✴✵✶")

# A saved session's craze id: a past run's, in the index and nowhere else.
SAVED_ID = "0199aaaa-bbbb-7ccc-8ddd-0000000000f1"


@pytest.fixture(autouse=True)
def _detached(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """The ordinary, detached craze: conftest's opt-out is lifted, and the
    suite's short idle exit seeded in the shared HOME."""
    monkeypatch.delenv("CRAZE_DETACH", raising=False)
    seed_host_idle_exit(tmp_path)


# The end of one frame of bubbletea's alt-screen renderer: every flush starts
# at the home position (ESC[H) and ends by putting the cursor on the frame's
# last row, ESC[<rows>;H. craze's own output never positions the cursor so.
_FRAME_END = re.compile(r"\x1b\[\d+;\d*H")


class Screen:
    """The terminal a PTYCraze draws on (80x24), with craze's output played
    into it: what each row holds now.

    It knows what bubbletea's renderer writes -- cursor moves, erasing to the
    end of a line or of the screen, carriage returns and line feeds, printable
    text one cell per character (every glyph the list draws is one cell) --
    and passes over the rest (colours, modes, OSC strings). Only whole frames
    are played (_FRAME_END): one frame can arrive in several reads of the pty,
    and a screen read between them would be half one frame and half the last.
    """

    def __init__(self, tui: PTYCraze, rows: int = 24, cols: int = 80) -> None:
        self.tui = tui
        self.nrows, self.ncols = rows, cols
        self.grid = [[" "] * cols for _ in range(rows)]
        self.row = self.col = 0
        self.saved = (0, 0)
        self.pos = 0
        self.decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
        self.pending = ""

    def rows(self) -> list[str]:
        """The screen now, one string per row, trailing blanks dropped."""
        self._feed()
        return ["".join(r).rstrip() for r in self.grid]

    def dump(self) -> str:
        return "\n".join(self.rows())

    def _feed(self) -> None:
        data = bytes(self.tui.buf[self.pos :])
        self.pos += len(data)
        self.pending += self.decoder.decode(data)
        ends = list(_FRAME_END.finditer(self.pending))
        if not ends:
            return
        text, self.pending = self.pending[: ends[-1].end()], self.pending[ends[-1].end() :]
        i, n = 0, len(text)
        while i < n:
            if text[i] == "\x1b":
                end = _escape_end(text, i) or n
                self._escape(text[i:end])
                i = end
                continue
            self._char(text[i])
            i += 1

    def _blank(self) -> list[str]:
        return [" "] * self.ncols

    def _escape(self, seq: str) -> None:
        if seq[1] == "7":
            self.saved = (self.row, self.col)
        elif seq[1] == "8":
            self.row, self.col = self.saved
        if seq[1] != "[":
            return
        final, params = seq[-1], seq[2:-1]
        if params[:1] in ("?", ">", "<", "="):
            return
        nums = [int(p) if p.isdigit() else 0 for p in params.split(";")] if params else []

        def arg(k: int, default: int) -> int:
            return nums[k] if k < len(nums) and nums[k] > 0 else default

        last_row, last_col = self.nrows - 1, self.ncols - 1
        if final in "Hf":
            self.row, self.col = min(last_row, arg(0, 1) - 1), min(last_col, arg(1, 1) - 1)
        elif final == "A":
            self.row = max(0, self.row - arg(0, 1))
        elif final == "B":
            self.row = min(last_row, self.row + arg(0, 1))
        elif final == "C":
            self.col = min(last_col, self.col + arg(0, 1))
        elif final == "D":
            self.col = max(0, self.col - arg(0, 1))
        elif final == "G":
            self.col = min(last_col, arg(0, 1) - 1)
        elif final == "d":
            self.row = min(last_row, arg(0, 1) - 1)
        elif final == "K":
            mode, line = (nums[0] if nums else 0), self.grid[self.row]
            span = {0: range(min(self.col, self.ncols), self.ncols), 1: range(min(self.col + 1, self.ncols))}
            for c in span.get(mode, range(self.ncols)):
                line[c] = " "
        elif final == "J":
            mode = nums[0] if nums else 0
            if mode == 0:
                self.grid[self.row][min(self.col, self.ncols) :] = [" "] * (self.ncols - min(self.col, self.ncols))
                cleared = range(self.row + 1, self.nrows)
            elif mode == 1:
                cleared = range(self.row)
            else:
                cleared = range(self.nrows)
            for r in cleared:
                self.grid[r] = self._blank()

    def _newline(self) -> None:
        self.row += 1
        if self.row >= self.nrows:
            self.grid.pop(0)
            self.grid.append(self._blank())
            self.row = self.nrows - 1

    def _char(self, ch: str) -> None:
        if ch == "\r":
            self.col = 0
        elif ch == "\n":
            self._newline()
        elif ch == "\b":
            self.col = max(0, self.col - 1)
        elif ch == "\t":
            self.col = min(self.ncols - 1, (self.col // 8 + 1) * 8)
        elif ord(ch) >= 0x20 and ord(ch) != 0x7F:
            if self.col >= self.ncols:
                self.col = 0
                self._newline()
            self.grid[self.row][self.col] = ch
            self.col += 1


def _escape_end(text: str, i: int) -> int | None:
    """Where the escape sequence at text[i] ends, or None when it is not
    whole: CSI up to its final byte, an OSC/DCS/APC/PM string up to BEL or
    ST, a charset designation's three bytes, any other escape's two."""
    n = len(text)
    if i + 1 >= n:
        return None
    kind = text[i + 1]
    if kind == "[":
        j = i + 2
        while j < n and 0x20 <= ord(text[j]) <= 0x3F:
            j += 1
        return None if j >= n else j + 1
    if kind in "]P_^X":
        j = i + 2
        while j < n:
            if text[j] == "\x07":
                return j + 1
            if text[j] == "\x1b":
                if j + 1 >= n:
                    return None
                if text[j + 1] == "\\":
                    return j + 2
            j += 1
        return None
    if kind in "()*+#%":
        return None if i + 2 >= n else i + 3
    return i + 2


# ------------------------------------------------------------------ helpers


def _wait_screen(screen: Screen, what: str, pred: Callable[[list[str]], bool], timeout: float = WAIT) -> list[str]:
    """The screen's rows once pred holds for them, within timeout: a bound of
    this step's own."""
    deadline = time.monotonic() + timeout
    while True:
        rows = screen.rows()
        if pred(rows):
            return rows
        if screen.tui.proc.poll() is not None:
            raise AssertionError(f"craze exited {screen.tui.proc.returncode} waiting for {what}:\n{screen.dump()}")
        if time.monotonic() >= deadline:
            raise AssertionError(f"timeout ({timeout}s) waiting for {what}:\n{screen.dump()}")
        time.sleep(0.05)


def _group(rows: list[str], label: str) -> list[str] | None:
    """The rows under the list's group header `label N ───`, None when there
    is no such group. The header's count must be the rows' own."""
    for i, row in enumerate(rows):
        m = re.match(rf"{re.escape(label)} (\d+) ─", row)
        if not m:
            continue
        body: list[str] = []
        for r in rows[i + 1 :]:
            if not r.strip():
                break
            body.append(r)
        assert int(m.group(1)) == len(body), f"group {label!r} counts {m.group(1)}, lists {body}"
        return body
    return None


def _headers(rows: list[str]) -> list[str]:
    """The list's group headers in order, by label."""
    return [m.group(1) for r in rows if (m := re.match(r"(\S.*?) \d+ ─", r))]


def _glyph(row: str) -> str:
    """A session row's state glyph: the cell after the selection mark."""
    return row[2:3]


def _selected(rows: list[str]) -> int | None:
    """The index of the selected line (the `❯` mark), None when none is."""
    return next((i for i, r in enumerate(rows) if r.startswith("❯")), None)


def _select(tui: PTYCraze, screen: Screen, what: str, pred: Callable[[str], bool]) -> list[str]:
    """Move the list's selection with ↑/↓ onto the line pred matches, one key
    at a time, each move's redraw bounded on its own; the line must be on
    screen already."""
    for _ in range(screen.nrows):
        rows = screen.rows()
        sel = _selected(rows)
        target = next((i for i, r in enumerate(rows) if pred(r)), None)
        assert sel is not None and target is not None, f"no line for {what} to select:\n{screen.dump()}"
        if target == sel:
            return rows
        tui.write(UP if target < sel else DOWN)
        _wait_screen(screen, f"the selection moving toward {what}", lambda rows, s=sel: _selected(rows) != s)
    raise AssertionError(f"the selection never reached {what}:\n{screen.dump()}")


def _hint(rows: list[str]) -> str:
    """The list's hint line: the screen's last row."""
    return rows[-1]


def _here(rows: list[str], title: str, screen: Screen) -> str:
    """The list's row of the session this terminal shows, titled title: idle,
    the cursor on it (the list opens there), its directory marked `· here`
    (clipped at 80 columns, `<dir> ·…`), and enter on it going back to it."""
    row = next((r for r in _group(rows, "idle") or [] if title in r), None)
    assert row is not None and row.startswith("❯") and _glyph(row) == IDLE, (
        f"no row here for {title!r}:\n{screen.dump()}"
    )
    assert re.search(r" \S+ ·(?: h(?:ere|\S*…)|…)", row), f"{title!r} is not marked here:\n{screen.dump()}"
    assert "enter back to it" in _hint(rows), screen.dump()
    return row


def _tui(craze_bin: Path, fake_agent_bin: Path, home: Path, workspace: Path, script: str = "echo") -> PTYCraze:
    """craze over the fake agent in workspace, HOME home: one terminal tab.
    Its agent names its session after the workspace (CRAZE_FAKE_SESSION_ID):
    the index keys a row by provider and that id, and two sessions under one
    id would be one row."""
    workspace.mkdir(exist_ok=True)
    env = {"HOME": str(home), "CRAZE_FAKE_SESSION_ID": f"fake-{workspace.name}"}
    return PTYCraze(craze_bin, fake_agent_bin, workspace, script=script, env_extra=env)


def _prompt(tui: PTYCraze, text: str) -> None:
    mark = tui.mark()
    tui.write(text.encode() + ENTER)
    tui.wait_contains_since(f"echo: {text}", mark, timeout=WAIT)


def _entry(home: Path, workspace: Path, timeout: float = WAIT) -> dict:
    """The ready registry entry of the host serving a session in workspace."""
    deadline = time.monotonic() + timeout
    seen: list[dict] = []
    while time.monotonic() < deadline:
        seen = _entries(home)
        for e in seen:
            if (
                e.get("ready")
                and e.get("crazeSessionId")
                and Path(e.get("workspace", "")).resolve() == workspace.resolve()
            ):
                return e
        time.sleep(0.05)
    raise AssertionError(f"no ready host serving {workspace} in the registry: {seen}")


def _wait_entry_gone(home: Path, entry: dict, timeout: float = WAIT) -> None:
    """entry's host process gone and its registry entry with it."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not pid_alive(entry["pid"]) and all(e["hostId"] != entry["hostId"] for e in _entries(home)):
            return
        time.sleep(0.05)
    raise AssertionError(f"host {entry['pid']} alive={pid_alive(entry['pid'])}, registry {_entries(home)}")


def _seed_saved(home: Path, workspace: Path, title: str) -> None:
    """A past run's row in HOME's index: a cursor session with a craze id,
    that ran in workspace -- saved, and running nowhere."""
    workspace.mkdir(exist_ok=True)
    index = home / ".craze" / "sessions.jsonl"
    index.parent.mkdir(parents=True, exist_ok=True)
    row = {
        "sessionId": "saved-1",
        "provider": "cursor",
        "cwd": str(workspace.resolve()),
        "title": title,
        "pinned": False,
        "createdAt": "2026-09-27T10:00:00Z",
        "updatedAt": "2026-09-27T10:00:00Z",
        "crazeId": SAVED_ID,
    }
    with index.open("a", encoding="utf-8") as f:
        f.write(json.dumps(row) + "\n")


def _open_list(tui: PTYCraze, screen: Screen, what: str, pred: Callable[[list[str]], bool]) -> list[str]:
    """← on the empty composer, and the list once it shows what pred wants."""
    tui.write(LEFT)
    return _wait_screen(screen, what, lambda rows: rows[0].startswith(" sessions  ") and pred(rows))


@contextmanager
def _stopped(pid: int) -> Iterator[None]:
    """pid SIGSTOPped for the block, SIGCONTed however it ends (so the
    cleanup's SIGTERM is heard)."""
    os.kill(pid, signal.SIGSTOP)
    try:
        yield
    finally:
        os.kill(pid, signal.SIGCONT)


# -------------------------------------------------------------------- cases


def test_the_list_groups_regroups_cancels_and_closes(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """AC8: two sessions in two directories and a saved one. `←` lists them in
    their groups -- the working one with the spinner and what it is doing, the
    idle one (this terminal's, `· here`) with `○` and its last reply, and
    `▸ saved · 1 not running`; `ctrl+s` groups them by directory and back;
    `ctrl+x` on the working row cancels its turn and clears its queue, so it
    settles idle and the queued prompt never runs; `ctrl+x` twice on that idle
    row closes it -- its host exits, its terminal goes back to its own list,
    and its row moves to saved."""
    home = tmp_path
    _seed_saved(home, home / "charlie", "the saved one")
    with (
        _tui(craze_bin, fake_agent_bin, home, home / "alpha") as a,
        _tui(craze_bin, fake_agent_bin, home, home / "bravo", script="hang-ack") as b,
    ):
        a.wait_contains("cursor", timeout=WAIT)
        b.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        # bravo's turn runs until it is cancelled, and a second prompt waits
        # in its queue behind it.
        b.write(b"bravo first" + ENTER)
        b.wait_contains("ack: bravo first", timeout=WAIT)
        mark = b.mark()
        b.write(b"bravo second" + ENTER)
        b.wait_contains_since("1 queued", mark, timeout=WAIT)
        bravo = _entry(home, home / "bravo")

        sa, sb = Screen(a), Screen(b)
        # Both hosts' answers: until its first, a host's row is drawn with
        # the working ones, `Connecting…` (X73).
        rows = _open_list(
            a,
            sa,
            "both running sessions listed as their hosts answered",
            lambda rows: any("Responding" in r for r in _group(rows, "working") or []) and _group(rows, "idle"),
        )
        assert rows[0].startswith(" sessions  2 running"), sa.dump()
        assert "1 working" in rows[0] and f"{IDLE} 1 idle" in rows[0], sa.dump()
        (working,) = _group(rows, "working")
        assert _glyph(working) in SPINNER and "Responding" in working and "bravo" in working, sa.dump()
        (idle,) = _group(rows, "idle")
        assert "echo: alpha one" in idle, sa.dump()
        _here(rows, "alpha one", sa)
        assert any(r.startswith("  ▸ saved · 1 not running ─") for r in rows), sa.dump()
        assert _headers(rows) == ["working", "idle"], sa.dump()

        # ctrl+s: grouped by directory -- the most urgent directory first --
        # and back.
        a.write(CTRL_S)
        rows = _wait_screen(sa, "grouping by directory", lambda rows: _group(rows, "~/alpha") is not None)
        assert _headers(rows) == ["~/bravo", "~/alpha"], sa.dump()
        assert "Responding" in _group(rows, "~/bravo")[0] and "alpha one" in _group(rows, "~/alpha")[0], sa.dump()
        assert "ctrl+s by state" in _hint(rows), sa.dump()
        a.write(CTRL_S)
        rows = _wait_screen(sa, "grouping by state again", lambda rows: _headers(rows) == ["working", "idle"])

        # ctrl+x on the working row: the turn is cancelled and the queue
        # cleared, so it settles idle and stays so.
        rows = _select(a, sa, "the working row", lambda r: "Responding" in r)
        assert "ctrl+x stop" in _hint(rows), sa.dump()
        a.write(CTRL_X)
        # Its answer (the note) and its row settling (the next snapshot) come
        # in either order.
        _wait_screen(
            sa,
            "the cancelled session idle, and ctrl+x's answer",
            lambda rows: (
                _group(rows, "working") is None
                and len(_group(rows, "idle") or []) == 2
                and _hint(rows).strip().startswith("stopped: ")
            ),
        )
        _wait_screen(
            sb, "bravo's queue cleared", lambda rows: "queued" not in rows[-1] and "cancelled" in "\n".join(rows)
        )
        # Settled, and staying so: the queue's prompt would have started a
        # turn that runs until cancelled -- working again, "ack: bravo second"
        # on its terminal. Past a roster tick and a drain, neither happened.
        time.sleep(1.5)
        rows = sa.rows()
        assert _group(rows, "working") is None, sa.dump()
        assert "ack: bravo second" not in "\n".join(sb.rows()), sb.dump()

        # ctrl+x twice on it, now idle: closed on its host. (The selection
        # stayed on its row as it moved group: it is held by identity.)
        _select(a, sa, "bravo's idle row", lambda r: "bravo" in r and "alpha" not in r)
        a.write(CTRL_X)
        _wait_screen(sa, "the close armed", lambda rows: "ctrl+x again closes it" in _hint(rows))
        a.write(CTRL_X)
        _wait_entry_gone(home, bravo)
        _wait_screen(sb, "bravo's terminal back on its list", lambda rows: "that session ended" in rows[-1])
        rows = _wait_screen(
            sa,
            "the closed session saved",
            lambda rows: (
                any(r.startswith("  ▸ saved · 2 not running ─") for r in rows) and _group(rows, "idle") is not None
            ),
        )
        (idle,) = _group(rows, "idle")
        assert "alpha one" in idle, sa.dump()
        assert rows[0].startswith(" sessions  1 running"), sa.dump()
        assert b.proc.poll() is None, "bravo's craze quit when its session was closed from another terminal"


def test_enter_opens_a_session_in_place_and_ctrl_d_leaves_them_running(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """AC8-AC9: `enter` on another session's row opens it in this terminal, in
    place -- the band names it (`title · provider · directory`, `← sessions`;
    the title is the row's: the agent's, else the index's -- the fake agent
    sets none, so its first prompt's), the status row names its directory, its
    transcript is on screen, and a prompt typed here
    reaches it (its own terminal shows the reply too). Back on the list the
    cursor starts on it, the session here. `ctrl+d` on the list quits craze
    and stops nothing: both hosts stay registered and alive, and the other
    terminal goes on."""
    home = tmp_path
    with (
        _tui(craze_bin, fake_agent_bin, home, home / "alpha") as a,
        _tui(craze_bin, fake_agent_bin, home, home / "bravo") as b,
    ):
        a.wait_contains("cursor", timeout=WAIT)
        b.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        _prompt(b, "bravo one")
        alpha, bravo = _entry(home, home / "alpha"), _entry(home, home / "bravo")

        sa = Screen(a)
        _open_list(a, sa, "both sessions idle", lambda rows: len(_group(rows, "idle") or []) == 2)
        _select(a, sa, "bravo's row", lambda r: "bravo one" in r)
        a.write(ENTER)
        rows = _wait_screen(
            sa, "bravo's session opened here", lambda rows: rows[0].startswith("─ bravo one · cursor · bravo ")
        )
        assert rows[0].endswith("← sessions ─"), sa.dump()
        # Its transcript comes with its first restore, and the status row
        # names its directory from the switch and its provider once it is up.
        _wait_screen(
            sa,
            "bravo's transcript, and bravo up",
            lambda rows: (
                any("echo: bravo one" in r for r in rows) and any(r.startswith("bravo │ cursor") for r in rows)
            ),
        )
        mark = b.mark()
        _prompt(a, "from alpha's terminal")
        b.wait_contains_since("echo: from alpha's terminal", mark, timeout=WAIT)

        rows = _open_list(a, sa, "the list, bravo here", lambda rows: len(_group(rows, "idle") or []) == 2)
        _here(rows, "bravo one", sa)
        assert any("alpha one" in r and " alpha " in r for r in _group(rows, "idle")), sa.dump()

        a.write(CTRL_D)
        assert a.wait_exit(timeout=5) == 0, sa.dump()
        # Nothing stopped is a fact that has to hold for a moment: half a
        # second is far past the client's own teardown.
        time.sleep(0.5)
        assert {e["hostId"] for e in _entries(home)} == {alpha["hostId"], bravo["hostId"]}, _entries(home)
        assert pid_alive(alpha["pid"]) and pid_alive(bravo["pid"])
        _prompt(b, "bravo goes on")


def test_an_unreachable_host_shows_a_question_mark(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """AC8: a host the registry lists that does not answer -- SIGSTOPped here --
    is its own group, `?` and `not answering`, counted as running and never
    shown as saved (its session has an index row, so it would be); `ctrl+x`
    on it says it is not answering. Once it answers again it is idle again."""
    home = tmp_path
    with (
        _tui(craze_bin, fake_agent_bin, home, home / "alpha") as a,
        _tui(craze_bin, fake_agent_bin, home, home / "bravo") as b,
    ):
        a.wait_contains("cursor", timeout=WAIT)
        b.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        _prompt(b, "bravo one")
        bravo = _entry(home, home / "bravo")

        sa = Screen(a)
        with _stopped(bravo["pid"]):
            rows = _open_list(a, sa, "bravo unreachable", lambda rows: _group(rows, "unreachable") is not None)
            (row,) = _group(rows, "unreachable")
            assert _glyph(row) == UNREACHABLE and "not answering" in row and "bravo" in row, sa.dump()
            assert rows[0].startswith(" sessions  2 running"), sa.dump()
            assert not any("saved" in r for r in rows), f"an unreachable session is listed saved:\n{sa.dump()}"
            _select(a, sa, "the unreachable row", lambda r: "not answering" in r)
            a.write(CTRL_X)
            _wait_screen(sa, "ctrl+x on it answered", lambda rows: "that session is not answering" in _hint(rows))
        # The roster asks a host that failed again only after its backoff,
        # which doubles up to 30 s: as long as the host was stopped, at most.
        _wait_screen(
            sa,
            "bravo answering again",
            lambda rows: _group(rows, "unreachable") is None and len(_group(rows, "idle") or []) == 2,
            timeout=WAIT + 30,
        )


def test_a_saved_session_resumes_in_place(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """§3.12, AC8: `enter` on `▸ saved` lists the saved sessions -- `·`, their
    title, provider and directory; `enter` on one resumes it in this
    terminal: a host is spawned to load it by its craze id in the directory it
    ran in (not this terminal's), the band names it, and a prompt reaches it.
    Back on the list it is running, `· here`, and no longer saved; the session
    this terminal left runs on."""
    home = tmp_path
    _seed_saved(home, home / "charlie", "the saved one")
    # permodel: a fake cursor that answers session/new and session/load both.
    with _tui(craze_bin, fake_agent_bin, home, home / "alpha", script="permodel") as a:
        a.wait_contains("cursor", timeout=WAIT)
        _prompt(a, "alpha one")
        alpha = _entry(home, home / "alpha")

        sa = Screen(a)
        _open_list(
            a, sa, "the saved group", lambda rows: any(r.startswith("  ▸ saved · 1 not running ─") for r in rows)
        )
        _select(a, sa, "the saved line", lambda r: "saved · 1 not running" in r)
        a.write(ENTER)
        _wait_screen(sa, "the saved group open", lambda rows: any("▾ saved · 1 not running" in r for r in rows))
        rows = _select(a, sa, "the saved row", lambda r: "the saved one" in r)
        row = rows[_selected(rows)]
        assert _glyph(row) == SAVED and "cursor" in row and "charlie" in row, sa.dump()
        assert "enter resume" in _hint(rows), sa.dump()

        a.write(ENTER)
        # Restoring (the status row says so) until its load is over, then up.
        _wait_screen(
            sa,
            "the saved session resumed here, and up",
            lambda rows: (
                rows[0].startswith("─ the saved one · cursor · charlie ")
                and any(r.startswith("charlie │ cursor") for r in rows)
            ),
        )
        entry = _entry(home, home / "charlie")
        assert entry["crazeSessionId"] == SAVED_ID, entry
        _prompt(a, "after the resume")

        rows = _open_list(a, sa, "the resumed session running", lambda rows: len(_group(rows, "idle") or []) == 2)
        _here(rows, "the saved one", sa)
        assert not any("saved ·" in r for r in rows), f"the resumed session is still listed saved:\n{sa.dump()}"
        assert pid_alive(alpha["pid"]), "the session this terminal left was stopped"
