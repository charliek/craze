"""The slash catalog, from the compiled binaries (plan 043 C5, T1-T4).

A session's host publishes the `/` menu as data: the attach snapshot's
`settings.slash` and, on change, a `meta` state delta carrying `slash`
(protocol.md's "Slash catalog"). These cases read it the way a remote client
does -- a raw JSON-RPC client over `craze bridge --hub`, spliced to the host of
a session the real hub created -- and run what it says:

- T1: the catalog an attach hands a client, for a fake-ACP session (grok's
  dialect, a disk skill, an agent command) and for a native session (against
  the repo's SSE fixture), and `capabilities.slashCatalog`.
- T2: the builtin rows run *as written* -- `run.method` and `run.param` /
  `run.setting` taken from the row itself -- and each effect is seen on the
  stream, not just in a reply.
- T3: one `available_commands_update` is one delta carrying commands, plugins
  and slash; a repeat of an identical one is none.
- T4: a disk skill created while the session sits idle reaches a client after
  the next attach and after a turn ends.

Every case runs under conftest's isolation and test_client_recipe's hermetic
recipe (its bridge, its private PATH, its Procs): the hub is real, the host is
a real `craze serve`, and its agent is craze-fake-agent as grok (a script that
advertises modes and an `aside`), or native against a loopback fixture. Every
wait is bounded and waits on its own precondition: a reply's id, an event that
satisfies a predicate. Absence is shown the only way a stream can show it: by an
event that must come after the thing that would have been sent.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

import conftest
from sse_fixture import SSEFixture, write_native_config
from test_client_recipe import (  # noqa: F401 -- recipe_procs is a fixture, used by name
    CREATE_WAIT,
    HELLO,
    WAIT,
    Bridge,
    Procs,
    private_path,
    recipe_env,
    recipe_procs,
)

# The builtin rows of a session whose provider is grok (or cursor) with modes
# and an aside: a fake grok advertises modes default/plan/ask, so the agent row
# runs mode "default". Descriptions are the TUI's own words.
_HELP = {"name": "help", "description": "Keybindings and commands", "source": "builtin", "whileWorking": True,
         "run": {"kind": "local", "local": "help"}}
_MODEL = {"name": "model", "description": "Switch model", "source": "builtin",
          "run": {"kind": "picker", "arg": "optional", "argHint": "<model> [effort]", "picker": "model"}}
_CONNECT = {"name": "connect", "description": "Connect a model provider: store its API key", "source": "builtin",
            "run": {"kind": "local", "local": "connect"}}
_CLEAR = {"name": "clear", "description": "Clear transcript", "source": "builtin", "whileWorking": True,
          "run": {"kind": "local", "local": "clear"}}
_TASKS = {"name": "tasks", "description": "Tasks panel: compact, expanded, hidden", "source": "builtin",
          "whileWorking": True, "run": {"kind": "local", "local": "tasks"}}
_THEME = {"name": "theme", "description": "Theme picker, or /theme <name>", "source": "builtin", "whileWorking": True,
          "run": {"kind": "local", "arg": "optional", "argHint": "<name>", "local": "theme"}}
_RENAME = {"name": "rename", "description": "Rename this session, /rename <title>", "source": "builtin",
           "whileWorking": True,
           "run": {"kind": "method", "method": "session.setTitle", "param": "title", "arg": "required",
                   "argHint": "<title>"}}
_BTW = {"name": "btw", "description": "Ask a side question (not saved): /btw <question>", "source": "builtin",
        "requires": "aside", "whileWorking": True,
        "run": {"kind": "aside", "param": "question", "arg": "required", "argHint": "<question>"}}
_EXIT = {"name": "exit", "description": "End this session", "source": "builtin", "requires": "stop",
         "whileWorking": True, "run": {"kind": "method", "method": "session.stop"}}


def _mode_row(name: str, value: str) -> dict:
    return {"name": name, "description": f"Set {name} mode", "source": "builtin",
            "run": {"kind": "method", "method": "session.set", "setting": {"kind": "mode", "value": value}}}


def _agent_row(name: str) -> dict:
    return {"name": name, "description": "Agent-advertised command", "source": "agent",
            "run": {"kind": "prompt", "arg": "optional"}}


def _skill_row(name: str, description: str) -> dict:
    return {"name": name, "description": description, "source": "skill", "run": {"kind": "prompt", "arg": "optional"}}


# ---------------------------------------------------------------- the rig


def write_skill(root: Path, name: str, description: str) -> None:
    """<root>/<name>/SKILL.md with frontmatter naming the skill."""
    d = root / name
    d.mkdir(parents=True, exist_ok=True)
    (d / "SKILL.md").write_text(f"---\nname: {name}\ndescription: {description}\n---\nDo {name}.\n", encoding="utf-8")


def hub_env(
    tmp_path: Path, craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, script: str = "grok-echo"
) -> dict[str, str]:
    """The hermetic recipe's environment and config.toml, with grok pointed at
    the fake agent (running `script`). Native needs nothing more here: a case
    that uses it writes the fixture's providers.toml under CRAZE_HOME/native."""
    env = recipe_env(private_path(tmp_path / "path", craze_bin, craze_fake_host_bin))
    env["CRAZE_FAKE_SCRIPT"] = script
    craze_home = Path(env["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'provider = "grok"\nhost_idle_exit = "30s"\n\n[agents]\ngrok = "{fake_agent_bin}"\n', encoding="utf-8"
    )
    return env


def hub_bridge(procs: Procs, env: dict[str, str]) -> Bridge:
    """A bridge to the hub (spawning it if this is the first), hello done."""
    bridge = procs.bridge(env)
    bridge.call(1, "hello", HELLO, timeout=3 * WAIT)
    return bridge


def create_session(hub: Bridge, cwd: Path, provider: str, request_id: str, id_: int = 2, **more: Any) -> dict:
    """session.create through the hub: the created session's roster row."""
    return hub.call(
        id_, "session.create", {"cwd": str(cwd), "provider": provider, "requestId": request_id, **more},
        timeout=CREATE_WAIT,
    )["session"]


class Client:
    """A raw client of one session: a bridge spliced to its host, attached.

    `notes` holds every notification since the attach, in order; `events` is
    the event notifications among them. A mark (`mark()`) is a position in
    `notes`, so a wait or a count can ask about what came after a step.
    """

    def __init__(self, procs: Procs, env: dict[str, str], row: dict) -> None:
        self.bridge = hub_bridge(procs, env)
        self.session_id = row["sessionId"]
        assert self.bridge.call(2, "session.connect", {"sessionId": row["hostId"]}) == {}
        self.host_hello = self.bridge.call(3, "hello", HELLO)
        self._next_id = 10
        self._next_command = 1
        self.attached: dict = {}

    def attach(self) -> dict:
        """session.attach; its result (info document, snapshot, ...)."""
        self.attached = self.call("session.attach", {})
        return self.attached

    def call(self, method: str, params: dict, mutating: bool = False) -> dict:
        params = {"sessionId": self.session_id, **params}
        if mutating:
            params["commandId"] = str(self._next_command)
            self._next_command += 1
        self._next_id += 1
        return self.bridge.call(self._next_id, method, params)

    def request(self, method: str, params: dict) -> dict:
        """A mutating call sent as a catalog row says: sessionId and a
        commandId added to the row's own params."""
        return self.call(method, params, mutating=True)

    @property
    def notes(self) -> list[dict]:
        return self.bridge.notes

    def mark(self) -> int:
        return len(self.bridge.notes)

    def metas(self, since: int = 0) -> list[dict]:
        """The `meta` events from position since, each as {seq, state}."""
        out = []
        for note in self.bridge.notes[since:]:
            if note.get("method") == "event" and note["params"]["event"].get("type") == "meta":
                out.append({"seq": note["params"]["seq"], "state": note["params"]["event"].get("state") or {}})
        return out

    def wait_meta(self, pred: Callable[[dict], bool], what: str, since: int = 0, timeout: float = WAIT) -> dict:
        """The first meta event from position since whose state satisfies pred."""
        return self.bridge.wait_for(
            lambda _notes: next((m for m in self.metas(since) if pred(m["state"])), None), what, timeout
        )

    def wait_note(self, pred: Callable[[dict], bool], what: str, since: int = 0, timeout: float = WAIT) -> dict:
        return self.bridge.wait_for(
            lambda notes: next((n for n in notes[since:] if pred(n)), None), what, timeout
        )


def snapshot_slash(attached: dict) -> list[dict]:
    return ((attached["snapshot"]["settings"].get("slash")) or {}).get("items") or []


def names(items: list[dict]) -> list[str]:
    return [row["name"] for row in items]


def has_row(name: str, source: str | None = None) -> Callable[[dict], bool]:
    """A state predicate: its slash section lists the row."""

    def pred(state: dict) -> bool:
        return any(
            row.get("name") == name and (source is None or row.get("source") == source)
            for row in (state.get("slash") or {}).get("items") or []
        )

    return pred


def folded_slash(client: Client, until: str, source: str | None = None) -> list[dict]:
    """The catalog a client holds once row `until` is in it: the snapshot's
    section with every slash delta after it folded (present replaces), waiting
    for the delta that brings the row when the snapshot raced it."""
    items = snapshot_slash(client.attached)
    if any(r["name"] == until and (source is None or r["source"] == source) for r in items):
        return items
    client.wait_meta(has_row(until, source), f"a slash section listing /{until}")
    for meta in client.metas():
        if "slash" in meta["state"]:
            items = meta["state"]["slash"].get("items") or []
    return items


def settle(client: Client, quiet: float = 0.5, limit: float = 10.0) -> None:
    """Return once the stream has been quiet for `quiet` seconds.

    This is isolation margin, not a correctness sleep: a skill scan takes about
    0.1 ms, so half a second is far more than any scan a previous step asked
    for needs to land. A case calls it before it creates a file, so that a scan
    still pending from an earlier trigger (the install's, an attach's, a
    commands update's) cannot find the file and stand in for the trigger under
    test. It is bounded by `limit`, and reads through the bridge's own reader.
    """
    deadline = time.monotonic() + limit
    seen = len(client.notes)
    while True:
        try:
            client.bridge.wait_for(lambda notes: True if len(notes) > seen else None, "any note", timeout=quiet)
        except AssertionError:
            return  # nothing came for `quiet` seconds
        seen = len(client.notes)
        assert time.monotonic() < deadline, f"the stream never went quiet for {quiet}s within {limit}s"


@pytest.fixture
def fixture_server():
    server = SSEFixture().start()
    try:
        yield server
    finally:
        server.close()


# ------------------------------------------------------------------ T1


def test_attach_snapshot_carries_the_acp_catalog(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T1 (ACP): an attach over the real socket to a fake-grok session hands the
    client the catalog -- builtins in the TUI's order under the session's gates
    (modes the agent advertised, `btw` and `exit` for the session's `aside` and
    `stop`), then the agent's own command from its available_commands_update,
    then the disk skill under the session's `.grok/skills` -- and the session
    info says `slashCatalog` is true. The exact rows are the contract a remote
    client renders, so they are compared whole, not by name."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    write_skill(work / ".grok" / "skills", "deploy", "Ship it to production")
    hub = hub_bridge(recipe_procs, env)
    row = create_session(hub, work, "grok", "t1-acp")

    client = Client(recipe_procs, env, row)
    attached = client.attach()
    assert attached["session"]["capabilities"]["slashCatalog"] is True, attached["session"]["capabilities"]
    assert row["row"]["capabilities"]["slashCatalog"] is True, row["row"]["capabilities"]
    # The snapshot itself holds every builtin and the disk skill: the install
    # scanned the skills before the attach. Only the agent's command, which
    # follows session/new's reply asynchronously, may come in a later delta.
    expect = [
        _HELP,
        _MODEL,
        _CLEAR,
        _TASKS,
        _THEME,
        _RENAME,
        _mode_row("plan", "plan"),
        _mode_row("ask", "ask"),
        _mode_row("agent", "default"),
        _BTW,
        _EXIT,
        _agent_row("research"),
        _skill_row("deploy", "Ship it to production"),
    ]
    snapshot = snapshot_slash(attached)
    assert [r for r in snapshot if r["name"] != "research"] == [r for r in expect if r["name"] != "research"], names(
        snapshot
    )
    # And the whole catalog, once research has arrived, is the full list.
    assert folded_slash(client, "research") == expect


def test_attach_snapshot_carries_the_native_catalog(
    craze_bin: Path,
    craze_fake_host_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    recipe_procs: Procs,
    fixture_server: SSEFixture,
) -> None:
    """T1 (native): the same over a native session against the SSE fixture. Its
    gates differ from grok's -- `connect` is a row (native stores keys), the
    mode row `agent` runs mode "agent", its own `/compact` is the one agent
    row -- and it has no disk skills. `slashCatalog` is true here too."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    write_native_config(Path(env["CRAZE_HOME"]) / "native", fixture_server.base_url)
    work = tmp_path / "work"
    work.mkdir()
    hub = hub_bridge(recipe_procs, env)
    row = create_session(hub, work, "native", "t1-native")

    client = Client(recipe_procs, env, row)
    attached = client.attach()
    assert attached["session"]["capabilities"]["slashCatalog"] is True, attached["session"]["capabilities"]
    caps = attached["session"]["capabilities"]
    items = snapshot_slash(attached)
    expect = [
        _HELP,
        _MODEL,
        _CONNECT,
        _CLEAR,
        _TASKS,
        _THEME,
        _RENAME,
        _mode_row("plan", "plan"),
        _mode_row("ask", "ask"),
        _mode_row("agent", "agent"),
        _BTW,
        _EXIT,
        # native's own command: it advertises /compact itself, as an agent row.
        {"name": "compact", "description": "Summarize the conversation so far to free context; /compact <what to keep>",
         "source": "agent", "run": {"kind": "prompt", "arg": "optional"}},
    ]
    assert items == expect, (names(items), caps)


# ------------------------------------------------------------------ T2


@pytest.fixture(params=["acp", "native"])
def run_rig(
    request: pytest.FixtureRequest,
    craze_bin: Path,
    craze_fake_host_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    recipe_procs: Procs,
) -> tuple[Client, dict, dict[str, dict]]:
    """An attached client of a fresh session (a fake grok, or native against a
    fixture), the session's roster row, and its builtin rows by name."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    if request.param == "native":
        server = SSEFixture().start()
        request.addfinalizer(server.close)
        write_native_config(Path(env["CRAZE_HOME"]) / "native", server.base_url)
    hub = hub_bridge(recipe_procs, env)
    provider = "grok" if request.param == "acp" else "native"
    row = create_session(hub, work, provider, f"t2-{request.param}")
    client = Client(recipe_procs, env, row)
    client.attach()
    builtins = {r["name"]: r for r in snapshot_slash(client.attached) if r["source"] == "builtin"}
    return client, row, builtins


def test_builtin_rows_run_as_the_catalog_writes_them(run_rig: tuple[Client, dict, dict[str, dict]]) -> None:
    """T2: the rows' own descriptors are enough to run them. `rename` -- the
    row's `run.method` with the argument in its `run.param` -- is a title
    state delta; `plan` -- `run.method` with `run.setting` verbatim -- a mode
    delta; `exit` -- `run.method` alone -- ends the session: the stream's
    `reset{session_closed}` and the host process gone. Each effect is read off
    the stream, after the call, never from the reply alone."""
    client, row, rows = run_rig

    run = rows["rename"]["run"]
    assert (run["kind"], run["method"], run["param"], run["arg"]) == ("method", "session.setTitle", "title", "required")
    mark = client.mark()
    client.request(run["method"], {run["param"]: "Renamed by its row"})
    delta = client.wait_meta(lambda s: s.get("title") == "Renamed by its row", "the title delta", since=mark)
    assert delta["seq"] > 0

    run = rows["plan"]["run"]
    assert (run["kind"], run["method"]) == ("method", "session.set")
    mark = client.mark()
    reply = client.request(run["method"], {"setting": run["setting"]})
    assert reply["value"] == "plan", reply
    client.wait_meta(lambda s: s.get("mode") == "plan", "the mode delta", since=mark)

    run = rows["exit"]["run"]
    assert run == {"kind": "method", "method": "session.stop"}
    host_pid = row["host"]["pid"]
    mark = client.mark()
    client.request(run["method"], {})
    closed = client.wait_note(
        lambda n: n.get("method") == "reset" and n["params"]["reason"] == "session_closed",
        "reset{session_closed}",
        since=mark,
        timeout=3 * WAIT,
    )
    assert closed["params"]["reason"] == "session_closed"
    deadline = time.monotonic() + 3 * WAIT
    while conftest.pid_alive(host_pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    assert not conftest.pid_alive(host_pid), f"the host ({host_pid}) outlived its session.stop"


# ------------------------------------------------------------------ T3


def _catalog_change(state: dict) -> bool:
    return any(k in state for k in ("commands", "plugins", "slash"))


def test_a_commands_update_is_one_delta_and_a_repeat_is_none(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T3: the fake agent re-sends its command list on a prompt that says so
    (`SET-COMMANDS a,b`). A changed list is exactly one state delta carrying
    `commands`, `plugins` and `slash` together, with the new command in all
    but plugins; the same list sent again is none; and a third, different
    list is one again. The repeat's absence is proved by order: the host reads
    the agent's updates in order, so any delta the repeat would have caused
    comes before the third list's, and between the first's and the third's
    there is no catalog-bearing delta at all."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    hub = hub_bridge(recipe_procs, env)
    row = create_session(hub, work, "grok", "t3")
    client = Client(recipe_procs, env, row)
    client.attach()
    folded_slash(client, "research")  # the install's own catalog is in place
    # An early commands update can publish /research around the install; let
    # that settle (isolation margin) so no late delta of the install's lands
    # after the mark and counts as a catalog delta of the steps below.
    settle(client)
    mark = client.mark()

    def send(text: str) -> None:
        reply = client.request("session.prompt", {"text": text, "mode": "queue"})
        assert reply.get("turn") or reply.get("queued"), reply

    send("SET-COMMANDS research,alpha")
    first = client.wait_meta(has_row("alpha", "agent"), "the delta listing /alpha", since=mark)
    state = first["state"]
    assert {"commands", "plugins", "slash"} <= set(state), sorted(state)
    assert [c["name"] for c in state["commands"]["commands"]] == ["research", "alpha"], state["commands"]
    assert names(state["slash"]["items"])[-2:] == ["research", "alpha"], names(state["slash"]["items"])

    # The repeat, then a different list as the sentinel behind it.
    send("SET-COMMANDS research,alpha")
    send("SET-COMMANDS research,beta")
    third = client.wait_meta(has_row("beta", "agent"), "the delta listing /beta", since=mark)
    assert {"commands", "plugins", "slash"} <= set(third["state"]), sorted(third["state"])
    between = [m for m in client.metas(mark) if _catalog_change(m["state"])]
    assert [m["seq"] for m in between] == [first["seq"], third["seq"]], [(m["seq"], sorted(m["state"])) for m in between]


# ------------------------------------------------------------------ T4


def test_an_idle_skill_arrives_after_an_attach_and_after_a_turn(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T4: grok's skill roots include `<cwd>/.grok/skills` (the fake agent
    advertises no skills of its own, and does not watch the disk as grok does).
    A SKILL.md created while the session sits idle (a) shows up once another
    client attaches -- an attach scans the disk: the observer attached all along
    gets a slash-only delta, and the new client's snapshot with that delta
    holds the row -- and (b) a second one, created later, shows up after a turn
    of the observer's ends. Both are bounded polls of the stream, never sleeps."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    roots = work / ".grok" / "skills"
    hub = hub_bridge(recipe_procs, env)
    row = create_session(hub, work, "grok", "t4")
    observer = Client(recipe_procs, env, row)
    observer.attach()
    folded_slash(observer, "research")
    assert "alpha" not in names(snapshot_slash(observer.attached))

    # (a) created idle. First let every scan the install and the commands
    # update asked for land, so none is pending to find the file.
    settle(observer)
    write_skill(roots, "alpha", "The first idle skill")
    mark = observer.mark()
    second = Client(recipe_procs, env, row)
    second.attach()
    delta = observer.wait_meta(has_row("alpha", "skill"), "a slash delta listing /alpha", since=mark)
    # It came through the attach's own scan: after the snapshot the attach took.
    assert delta["seq"] > second.attached["snapshot"]["seq"], (delta["seq"], second.attached["snapshot"]["seq"])
    assert sorted(delta["state"]) == ["slash"], sorted(delta["state"])
    alpha = next(r for r in delta["state"]["slash"]["items"] if r["name"] == "alpha")
    assert alpha == _skill_row("alpha", "The first idle skill"), alpha
    # The second client's catalog -- snapshot, then its own deltas -- has it.
    assert any(r["name"] == "alpha" for r in folded_slash(second, "alpha", "skill"))

    # (b) a turn ends. The attach's scan is done once the stream is quiet.
    settle(observer)
    write_skill(roots, "beta", "The second idle skill")
    # Nothing has asked for a scan since the file appeared.
    assert not [m for m in observer.metas() if has_row("beta")(m["state"])]
    mark = observer.mark()
    reply = observer.request("session.prompt", {"text": "hello", "mode": "queue"})
    assert reply.get("turn") or reply.get("queued"), reply
    delta = observer.wait_meta(has_row("beta", "skill"), "a slash delta listing /beta", since=mark)
    assert sorted(delta["state"]) == ["slash"], sorted(delta["state"])
    # The turn's end is what asked for it: the `done` event precedes the delta.
    done = observer.wait_note(
        lambda n: n.get("method") == "event" and n["params"]["event"].get("type") == "done",
        "the turn's done event",
        since=mark,
    )
    assert done["params"]["seq"] < delta["seq"], (done["params"]["seq"], delta["seq"])
    items = delta["state"]["slash"]["items"]
    assert [r["name"] for r in items if r["source"] == "skill"] == ["alpha", "beta"], names(items)
