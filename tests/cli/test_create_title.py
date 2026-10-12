"""A title at session.create, and `craze new --title` (plan 043 C5, T5 and T6).

`session.create` takes a `title` where the hub's `hello` says `createTitle`
(protocol.md, "session.create"): the session is named from its start, pinned,
and the create's idempotency hash covers the title as normalized. `craze new
--title` is the CLI's way to it, with and without `--wait`.

These cases drive the compiled binaries through test_client_recipe's hermetic
recipe and test_slash_catalog's rig on top of it: a real hub (the first bridge
spawns it), real `craze serve` hosts whose agent is craze-fake-agent as grok,
and the CLI run under the recipe's own environment, so that it reaches the very
same hub and the same registry. Every wait is bounded and waits on its own
precondition.
"""

from __future__ import annotations

import json
import re
import subprocess
import time
from pathlib import Path


from test_client_recipe import CREATE_WAIT, HELLO, WAIT, Bridge, Procs, recipe_procs  # noqa: F401
from test_hub import _ps_rows
from sse_fixture import SSEFixture, write_native_config
from test_slash_catalog import Client, create_session, hub_bridge, hub_env

TITLE_RAW = "  Fix the\tlogin   bug \u200b"
TITLE = "Fix the login bug"


def _craze(env: dict[str, str], *args: str) -> subprocess.CompletedProcess[bytes]:
    """The recipe's own craze, with no terminal, in the recipe's environment:
    the same hub, registry and CRAZE_HOME as the bridges."""
    return subprocess.run(
        [str(Path(env["PATH"]) / "craze"), *args],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        env=env,
        timeout=6 * WAIT,
    )


def _ps_title(env: dict[str, str], short: str, want: str) -> dict[str, str]:
    """`craze ps`'s row for the session, once its TITLE is want: the index
    row is written by the host just after the session's start, so ps can
    precede it by an instant."""
    deadline = time.monotonic() + WAIT
    while True:
        out = _craze(env, "ps")
        assert out.returncode == 0 and out.stderr == b"", out
        rows = {r["SESSION"]: r for r in _ps_rows(out.stdout)}
        row = rows.get(short)
        if row is not None and row["TITLE"] == want:
            return row
        assert time.monotonic() < deadline, f"craze ps never showed {short} titled {want!r}: {rows}"
        time.sleep(0.05)


def _error(bridge: Bridge, id_: int, method: str, params: dict) -> dict:
    """The error a request is refused with (the test fails on a result)."""
    deadline = time.monotonic() + CREATE_WAIT
    bridge.send({"jsonrpc": "2.0", "id": id_, "method": method, "params": params})
    msg = bridge.reply(id_, deadline)
    assert "error" in msg, f"{method} was not refused: {msg}"
    return msg["error"]


def _listed(hub: Bridge, id_: int) -> dict[str, dict]:
    """The hub's sessions.list, by session id."""
    return {r["sessionId"]: r for r in hub.call(id_, "sessions.list", {})["sessions"]}


def _upserts(notes: list[dict], session_id: str) -> list[dict]:
    """Every roster upsert naming the session, in order."""
    return [
        u for n in notes if n.get("method") == "roster" for u in n["params"]["upserts"] if u["sessionId"] == session_id
    ]


# ------------------------------------------------------------------ T5


def test_create_with_a_title_names_the_session_from_its_start(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T5: a hub create with `title`. The hub's hello says `createTitle`. The
    first roster row of the session -- every row a subscriber's upserts carried,
    the create's own row, and a fresh `sessions.list` -- carries the title as
    normalized (escapes, tabs, runs of whitespace and the zero-width space gone,
    the ends trimmed), and `craze ps` shows it. A retry of the create under the
    same `requestId` whose title normalizes the same joins the session; another
    title, and no title, are `request_conflict`; a title of only whitespace is
    `bad_request` and creates nothing."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    hub = recipe_procs.bridge(env)
    hello = hub.call(1, "hello", HELLO, timeout=3 * WAIT)
    assert hello["capabilities"]["createTitle"] is True, hello["capabilities"]

    # A subscriber, so the very first upsert is seen as it was published.
    watcher = hub_bridge(recipe_procs, env)
    assert watcher.call(2, "sessions.subscribe", {})["sessions"] == []

    row = create_session(hub, work, "grok", "t5-title", title=TITLE_RAW)
    session_id = row["sessionId"]
    assert row["row"]["title"] == TITLE, row["row"]
    # An upsert may come before the hub has read the session's own row (it then
    # has none, registry facts only); every row it does carry has the title --
    # none of them is ever untitled.
    watcher.wait_for(
        lambda notes: next((u for u in _upserts(notes, session_id) if "row" in u), None),
        "a roster upsert carrying the session's row",
    )
    rows = [u["row"] for u in _upserts(watcher.notes, session_id) if "row" in u]
    assert rows and {r.get("title") for r in rows} == {TITLE}, rows
    assert _listed(watcher, 3)[session_id]["row"]["title"] == TITLE

    short = session_id[-8:]
    assert _ps_title(env, short, TITLE)["PROVIDER"] == "grok"

    # The retry joins: the same session, however the title is spelled so long
    # as it normalizes the same.
    again = create_session(hub, work, "grok", "t5-title", id_=3, title=TITLE)
    assert again["sessionId"] == session_id and again["hostId"] == row["hostId"], again
    assert len(_listed(watcher, 4)) == 1

    # The same id and params with another title, and with none: refused.
    for id_, more in ((4, {"title": "Another title"}), (5, {})):
        err = _error(hub, id_, "session.create", {"cwd": str(work), "provider": "grok", "requestId": "t5-title", **more})
        assert (err["data"]["code"], err["data"]["reason"]) == ("bad_request", "request_conflict"), err

    # Blank is no title: bad_request, and no session behind it.
    for id_, blank in ((6, "   "), (7, "\u200b"), (8, "")):
        err = _error(
            hub, id_, "session.create", {"cwd": str(work), "provider": "grok", "requestId": f"t5-blank-{id_}", "title": blank}
        )
        assert (err["data"]["code"], err["data"]["reason"]) == ("bad_request", "bad_request"), err
    assert list(_listed(watcher, 9)) == [session_id]


def _state_title(client: Client) -> str | None:
    """The session's title as a fresh session.snapshot reports it."""
    return client.call("session.snapshot", {})["snapshot"]["settings"].get("title")


def _run_turn(client: Client, text: str) -> int:
    """Send a prompt and return the seq of the `done` event that ends its turn."""
    mark = client.mark()
    reply = client.request("session.prompt", {"text": text, "mode": "queue"})
    assert reply.get("turn") or reply.get("queued"), reply
    done = client.wait_note(
        lambda n: n.get("method") == "event" and n["params"]["event"].get("type") == "done",
        f"the end of the turn {text!r}",
        since=mark,
        timeout=2 * WAIT,
    )
    return done["params"]["seq"]


def test_a_created_title_resists_the_agents_own(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T5 (pinned): a title given at create is pinned -- an agent's
    session_info_update does not replace it. The fake agent sends one on a
    prompt `SET-TITLE agent says`. The titled session keeps its title in
    session.snapshot, in the hub's sessions.list and in `craze ps`, while the very
    same prompt renames an untitled session, which is the control showing the
    directive reaches craze and would have changed the title."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    hub = hub_bridge(recipe_procs, env)
    titled = create_session(hub, work, "grok", "pin-titled", title="Created title")
    plain = create_session(hub, work, "grok", "pin-plain", id_=3)

    control = Client(recipe_procs, env, plain)
    control.attach()
    mark = control.mark()
    _run_turn(control, "SET-TITLE agent says")
    control.wait_meta(lambda s: s.get("title") == "agent says", "the untitled session taking the agent's title", since=mark)
    assert _state_title(control) == "agent says"

    client = Client(recipe_procs, env, titled)
    client.attach()
    _run_turn(client, "SET-TITLE agent says")
    # The agent's update is read before its turn starts, so it has been applied
    # (or refused) by the turn's end.
    assert _state_title(client) == "Created title"
    assert not [m for m in client.metas() if m["state"].get("title") == "agent says"]
    listed = _listed(hub, 4)
    assert listed[titled["sessionId"]]["row"]["title"] == "Created title", listed
    assert listed[plain["sessionId"]]["row"]["title"] == "agent says", listed
    assert _ps_title(env, titled["sessionId"][-8:], "Created title")["TITLE"] == "Created title"
    _ps_title(env, plain["sessionId"][-8:], "agent says")


def test_a_created_title_resists_natives_first_prompt_title(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T5 (native): a native session titles itself from its first prompt.
    Given a title at create it keeps it through a turn, while an untitled
    native session takes the prompt as its title in `craze ps` (the control)."""
    server = SSEFixture().start()
    try:
        server.set_ok(text_parts=["fine"])
        env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
        write_native_config(Path(env["CRAZE_HOME"]) / "native", server.base_url)
        work = tmp_path / "work"
        work.mkdir()
        hub = hub_bridge(recipe_procs, env)
        titled = create_session(hub, work, "native", "npin-titled", title="Native created")
        plain = create_session(hub, work, "native", "npin-plain", id_=3)
        for row in (titled, plain):
            client = Client(recipe_procs, env, row)
            client.attach()
            _run_turn(client, "name me from this prompt")
        _ps_title(env, plain["sessionId"][-8:], "name me from this prompt")
        assert _ps_title(env, titled["sessionId"][-8:], "Native created")["TITLE"] == "Native created"
        assert _listed(hub, 4)[titled["sessionId"]]["row"]["title"] == "Native created"
    finally:
        server.close()


# ------------------------------------------------------------------ T6


def test_new_title_names_the_session_from_its_start(
    craze_bin: Path, craze_fake_host_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """T6: `craze new --title x` -- through the hub -- and `craze new --wait
    --title x` -- the host started directly, no hub asked -- both name the
    session from its start: the hub's own `sessions.list` row and `craze ps`
    carry the title, and with `--wait` the first prompt (which would have been
    the title) does not replace it."""
    env = hub_env(tmp_path, craze_bin, craze_fake_host_bin, fake_agent_bin)
    work = tmp_path / "work"
    work.mkdir()
    hub = hub_bridge(recipe_procs, env)

    out = _craze(env, "new", "-C", str(work), "--provider", "grok", "--title", "  Plain   new ", "the first prompt")
    assert out.returncode == 0 and out.stderr == b"", out
    m = re.fullmatch(rb"started ([0-9a-f]{8}) in (.+)\n", out.stdout)
    assert m is not None, out.stdout
    plain = m.group(1).decode()
    _ps_title(env, plain, "Plain new")

    out = _craze(
        env, "new", "--wait", "--json", "-C", str(work), "--provider", "grok", "--title", "Waited new", "the second prompt"
    )
    assert out.returncode == 0 and out.stderr == b"", out
    res = json.loads(out.stdout)
    assert res["status"] == "completed" and res["message"] == "echo: the second prompt", res
    waited = res["short"]
    _ps_title(env, waited, "Waited new")

    titles = {sid[-8:]: r["row"]["title"] for sid, r in _listed(hub, 2).items()}
    assert titles == {plain: "Plain new", waited: "Waited new"}, titles

    out = _craze(env, "stop", waited)
    assert out.returncode == 0 and out.stdout == f"stopped {waited}\n".encode(), out
