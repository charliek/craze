"""Plan 044: the Anthropic provider from the outside, on the compiled binary (A6).

Every case runs the real craze against anthropic_fixture's loopback Anthropic
API, reached through the test override CRAZE_TEST_ANTHROPIC_API set in the
child's environment alone (conftest.py fences it for everything else), with the
shipped catalog in a temp CRAZE_HOME and ANTHROPIC_CANARY as the key:

- `craze auth login anthropic` stores the key and fetches the account's model
  list with it, following its pages, into native/anthropic-models.json, bound
  to the key by its id and never holding it;
- a two-step tool loop on anthropic/claude-haiku-5-5 replays step one's signed
  thinking in step two byte for byte, in wire order, and every request carries
  the thinking-binding beta, block_binding, display "summarized",
  output_config.effort, both cache breakpoints (the system prompt's and the
  top-level cache_control) and none of the fields craze never sends;
- the key-echo modes -- a 401 body and an SSE error event repeating the key,
  for the turn and for login's list -- never print it, journal it or write it
  anywhere but providers.toml.

A failing check never prints the key either: it fails through no_canary, with
the text masked and no traceback.
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
from collections.abc import Iterator
from pathlib import Path

import pytest

from anthropic_fixture import (
    ANTHROPIC_CANARY,
    ANTHROPIC_ENV,
    BINDING_BETA,
    AnthropicFixture,
    Reply,
    Text,
    Thinking,
    ToolUse,
)

KEY = "<CANARY>"

# Fields craze never sends (plan 044 §3.2).
FORBIDDEN = ("temperature", "top_p", "top_k", "stop_sequences", "metadata", "speed", "fallbacks", "inference_geo")


@pytest.fixture
def anthropic_api() -> Iterator[AnthropicFixture]:
    api = AnthropicFixture().start()
    try:
        yield api
    finally:
        api.close()


def masked(text: str) -> str:
    return text.replace(ANTHROPIC_CANARY, KEY)


def no_canary(what: str, where: str, text: str) -> None:
    """Fail when text holds the key, masked, with no traceback (whose frames
    would show the arguments holding it)."""
    at = text.find(ANTHROPIC_CANARY)
    if at >= 0:
        pytest.fail(f"{what}: the key leaked into {where} at index {at}; masked it reads:\n{masked(text)}", pytrace=False)


def craze_home() -> Path:
    return Path(os.environ["CRAZE_HOME"])


def native_dir() -> Path:
    return craze_home() / "native"


def run_craze(
    craze_bin: Path, api: AnthropicFixture, *args: str, stdin: str = "", env: dict[str, str] | None = None, timeout: float = 20
) -> subprocess.CompletedProcess[str]:
    child = os.environ.copy()
    child[ANTHROPIC_ENV] = api.base_url
    child.update(env or {})
    proc = subprocess.run(
        [str(craze_bin), *args], input=stdin, capture_output=True, text=True, env=child, timeout=timeout, check=False
    )
    what = " ".join(["craze", *args])
    no_canary(what, "stdout", proc.stdout)
    no_canary(what, "stderr", proc.stderr)
    return proc


def login(craze_bin: Path, api: AnthropicFixture) -> subprocess.CompletedProcess[str]:
    return run_craze(craze_bin, api, "auth", "login", "anthropic", stdin=ANTHROPIC_CANARY + "\n")


def files_holding(root: Path, needle: bytes) -> list[Path]:
    return sorted(p for p in root.rglob("*") if p.is_file() and needle in p.read_bytes())


def key_only_in(allowed: list[Path]) -> None:
    """The key is in allowed and no other file under CRAZE_HOME or HOME
    (compared outside an assert: pytest's rewriting would print it)."""
    holding = files_holding(craze_home(), ANTHROPIC_CANARY.encode()) + files_holding(
        Path(os.environ["HOME"]), ANTHROPIC_CANARY.encode()
    )
    if holding != allowed:
        pytest.fail(f"the key is in {holding}; want it in {allowed} alone", pytrace=False)


def events_of(stdout: str) -> list[dict]:
    return [json.loads(line) for line in stdout.splitlines() if line.strip()]


def key_id(key: str) -> str:
    return hashlib.sha256(key.strip().encode()).hexdigest()[:12]


def test_auth_login_fetches_the_list(craze_bin: Path, anthropic_api: AnthropicFixture) -> None:
    """§3.6 a: login stores the key, then fetches the account's list with it --
    both pages, the second after the first's last id -- and writes it bound to
    the key's id, 0600, without the key; `auth list` shows the stored key and
    logout keeps the list. The control: every list request carried the key."""
    proc = login(craze_bin, anthropic_api)
    assert proc.returncode == 0, masked(proc.stderr)
    providers = native_dir() / "providers.toml"
    assert proc.stdout == f"Saved the Anthropic key in {providers}.\nFetched 3 Anthropic models.\n", masked(proc.stdout)
    assert proc.stderr == "", masked(proc.stderr)

    pages = anthropic_api.requests_to("/v1/models")
    assert [r.method for r in pages] == ["GET", "GET"], len(pages)
    if not all(r.key == ANTHROPIC_CANARY for r in pages):
        pytest.fail("a list request did not carry the saved key", pytrace=False)
    assert "after_id" not in pages[0].query and pages[1].query.get("after_id") == ["claude-opus-5-5"], [p.query for p in pages]

    listing = native_dir() / "anthropic-models.json"
    assert listing.stat().st_mode & 0o777 == 0o600
    doc = json.loads(listing.read_text(encoding="utf-8"))
    assert doc["version"] == 1 and doc["key_id"] == key_id(ANTHROPIC_CANARY)
    assert [m["id"] for m in doc["models"]] == ["claude-haiku-5-5", "claude-opus-5-5", "claude-opus-6"]
    key_only_in([providers])

    proc = run_craze(craze_bin, anthropic_api, "auth", "list")
    rows = [line.split() for line in proc.stdout.splitlines()]
    assert ["Anthropic", "anthropic", "stored", "key"] in rows, masked(proc.stdout)
    proc = run_craze(craze_bin, anthropic_api, "auth", "logout", "anthropic")
    assert (proc.returncode, proc.stdout) == (0, "Removed the stored Anthropic key.\n"), masked(proc.stdout + proc.stderr)
    assert listing.exists(), "logout removed the account's list"


def test_auth_login_list_key_echo(craze_bin: Path, anthropic_api: AnthropicFixture) -> None:
    """P13: a list request the account refuses with a 401 that repeats the key
    is one note on stderr naming the status, never the key (run_craze fails the
    case on it in either stream); the key stays saved, the login exits 0 and
    no list is written. The control: the request carried the key, so the
    note's silence is the scrubber's."""
    anthropic_api.models_mode = "http_error"
    proc = login(craze_bin, anthropic_api)
    assert proc.returncode == 0, masked(proc.stderr)
    assert proc.stdout.startswith("Saved the Anthropic key in "), masked(proc.stdout)
    assert "Fetched" not in proc.stdout
    assert proc.stderr.startswith("note: the Anthropic model list could not be fetched (") and "401" in proc.stderr, masked(proc.stderr)
    assert proc.stderr.endswith("; a native session fetches it when it opens\n"), masked(proc.stderr)
    pages = anthropic_api.requests_to("/v1/models")
    if len(pages) != 1 or pages[0].key != ANTHROPIC_CANARY:
        pytest.fail(f"{len(pages)} list requests, or one without the key", pytrace=False)
    assert not (native_dir() / "anthropic-models.json").exists()
    key_only_in([native_dir() / "providers.toml"])


def test_tool_loop_replays_signed_thinking(craze_bin: Path, tmp_path: Path, anthropic_api: AnthropicFixture) -> None:
    """§3.3, A6: a two-step tool loop on anthropic/claude-haiku-5-5. Step one
    streams [signed thinking, text, tool_use]; step two's request replays them
    in that order, the thinking's text and signature byte for byte (raw in
    the body, never HTML-escaped), the tool's result after them; and both
    requests carry the binding beta, block_binding, display "summarized",
    output_config.effort (Haiku 5.5's default, medium), the system prompt's
    breakpoint and the top-level cache_control, and no forbidden field. The
    session found the list login wrote current: no list request of its own."""
    assert login(craze_bin, anthropic_api).returncode == 0
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "notes.txt").write_text("alpha\n", encoding="utf-8")
    thinking = Thinking(
        text="Read <notes.txt> & answer with what it says.",
        signature="EqQBCkYIBxgCKkDp4+/q9Zr0sXo1LwN3mT8vHq2uYb6cJdGfKs5aWe/RtUi0OyPlN7xMQ==",
    )
    call = ToolUse(id="toolu_01AbCdEf", name="read", input={"filePath": "notes.txt"})
    anthropic_api.set_script(
        [
            Reply(blocks=(thinking, Text("Reading it."), call), stop_reason="tool_use"),
            Reply(blocks=(Text("It says alpha."),), stop_reason="end_turn"),
        ]
    )
    proc = run_craze(
        craze_bin,
        anthropic_api,
        "prompt",
        "--json",
        "--provider",
        "native",
        "--model",
        "anthropic/claude-haiku-5-5",
        "--workspace",
        str(ws),
        "What does notes.txt say?",
        timeout=60,
    )
    assert proc.returncode == 0, masked(proc.stdout + proc.stderr)
    events = events_of(proc.stdout)
    assert "".join(e.get("text", "") for e in events if e.get("type") == "text").endswith("It says alpha."), events
    assert events[-1].get("type") == "done" and events[-1].get("stopReason") == "end_turn", events[-1]

    turns = anthropic_api.requests_to("/v1/messages")
    assert len(turns) == 2, len(turns)
    first, second = turns
    assert len(anthropic_api.requests_to("/v1/models")) == 2, "the session fetched the list login had just written"
    for i, req in enumerate((first, second)):
        if req.key != ANTHROPIC_CANARY:
            pytest.fail(f"request {i} did not carry the key", pytrace=False)
        shown = {k: v for k, v in req.headers.items() if k != "x-api-key"}  # a failure never prints the key
        assert req.headers.get("anthropic-beta") == BINDING_BETA, shown
        assert req.headers.get("anthropic-version") == "2023-06-01", shown
        body = req.body
        assert body["model"] == "claude-haiku-5-5" and body["stream"] is True
        assert body["thinking"] == {
            "type": "adaptive",
            "display": "summarized",
            "block_binding": {"prefix_mismatch_behavior": "drop_block"},
        }, body["thinking"]
        assert body["output_config"] == {"effort": "medium"}, body.get("output_config")
        assert body["cache_control"] == {"type": "ephemeral"}, body.get("cache_control")
        assert body["system"] and body["system"][-1].get("cache_control") == {"type": "ephemeral"}, body["system"][-1]
        assert all("cache_control" not in block for block in body["system"][:-1])
        assert [k for k in FORBIDDEN if k in body] == [], sorted(body)
        assert b"eager_input_streaming" not in req.raw

    # The control: the first request holds the prompt alone, so what the
    # second carries before the tool's result is step one replayed.
    assert [m["role"] for m in first.body["messages"]] == ["user"], first.body["messages"]
    messages = second.body["messages"]
    assert [m["role"] for m in messages] == ["user", "assistant", "user"], [m["role"] for m in messages]
    assert messages[1]["content"] == [
        {"type": "thinking", "thinking": thinking.text, "signature": thinking.signature},
        {"type": "text", "text": "Reading it."},
        {"type": "tool_use", "id": call.id, "name": call.name, "input": call.input},
    ], messages[1]["content"]
    for raw in (json.dumps(thinking.text, ensure_ascii=False), json.dumps(thinking.signature)):
        assert raw.encode() in second.raw, f"{raw} is not in the request as it streamed"
    results = [b for b in messages[2]["content"] if b.get("type") == "tool_result"]
    assert [r["tool_use_id"] for r in results] == [call.id], messages[2]
    assert "alpha" in json.dumps(results[0]["content"]), results[0]
    key_only_in([native_dir() / "providers.toml"])


@pytest.mark.parametrize("mode", ["http_error", "sse_error"])
def test_turn_key_echo_never_printed(craze_bin: Path, tmp_path: Path, anthropic_api: AnthropicFixture, mode: str) -> None:
    """P13, A6: a turn the API fails with an error repeating the key -- a 401
    body, or an `error` event in a 200 stream -- ends in an error that never
    shows it: not on stdout or stderr (run_craze), not in the journal, not in
    any file. The key is exported alone here (env-only), so no file may hold
    it. The control: the request carried the key, so its absence is the
    scrubber's."""
    anthropic_api.messages_mode = mode
    ws = tmp_path / "ws"
    ws.mkdir()
    proc = run_craze(
        craze_bin,
        anthropic_api,
        "prompt",
        "--json",
        "--provider",
        "native",
        "--model",
        "anthropic/claude-haiku-5-5",
        "--workspace",
        str(ws),
        "hi",
        env={"ANTHROPIC_API_KEY": ANTHROPIC_CANARY},
        timeout=60,
    )
    turns = anthropic_api.requests_to("/v1/messages")
    if len(turns) != 1 or turns[0].key != ANTHROPIC_CANARY:
        pytest.fail(f"{len(turns)} turn requests, or one without the key", pytrace=False)
    assert proc.returncode == 1, masked(proc.stdout + proc.stderr)
    errors = [e for e in events_of(proc.stdout) if e.get("type") == "error"]
    assert errors, masked(proc.stdout)
    if mode == "http_error":
        assert "Anthropic rejected the API key" in errors[0]["message"], masked(errors[0]["message"])
    journals = sorted((craze_home() / "journal").glob("*/*.jsonl"))
    assert journals, "the session journaled nothing, so the check below would prove nothing"
    key_only_in([])
