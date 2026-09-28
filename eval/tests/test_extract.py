"""Per-harness answer extraction on recorded output samples (plan 029 AC-A1).

The samples under tests/samples/ are real stdout from the C1 smoke (smoke-fix on
glm-5.3-flash for craze, gx and opencode; on deepseek-v4p1-flash for codex), key-scanned
before they were copied here.
"""

from __future__ import annotations

import json
from pathlib import Path

from crazeeval.runners.base import read_ndjson
from crazeeval.runners.codex import extract_codex
from crazeeval.runners.craze import extract_craze
from crazeeval.runners.gx import extract_gx
from crazeeval.runners.opencode import extract_opencode

S = Path(__file__).parent / "samples"


def test_craze_text_after_the_last_tool_event():
    ev = read_ndjson(S / "craze-smoke-fix.jsonl")
    ex = extract_craze(ev)
    assert ex.answer == (
        "Fixed: `total()` used `range(len(lines) - 1)`, skipping the last line. Now it iterates over all lines "
        "(inventory.py:23). All 3 tests pass."
    )
    assert "stop=end_turn" in ex.notes
    ex2 = extract_craze(read_ndjson(S / "craze-smoke-explain.jsonl"))
    assert "KESTREL-7" in ex2.answer


def test_craze_ignores_sub_agent_text_and_leads_with_a_plan():
    ev = [
        {"type": "text", "text": "Looking."},
        {"type": "tool", "name": "agent", "status": "completed"},
        {"type": "text", "text": "child says hi", "agent": "a1"},
        {"type": "tool", "name": "read", "status": "completed", "agent": "a1"},
        {"type": "text", "text": "Final answer."},
        {"type": "done", "stopReason": "end_turn"},
    ]
    assert extract_craze(ev).answer == "Final answer."
    assert extract_craze(ev, plan_text="# Plan\n1. do it").answer == "# Plan\n1. do it\n\nFinal answer."


def test_gx_terminal_result_line():
    ev = read_ndjson(S / "gx-smoke-fix.jsonl")
    result = [e for e in ev if e.get("type") == "result"][-1]
    ex = extract_gx(ev)
    assert ex.answer == result["result"].strip() and ex.answer
    assert "`7`" in ex.answer
    assert ex.extra["subtype"] == "success" and ex.extra["permissionMode"] == "bypassPermissions"
    assert "run_terminal_command" in ex.extra["tools"]


def test_gx_error_result_has_no_answer():
    ev = [{"type": "result", "subtype": "error_during_execution", "is_error": True, "errors": ["boom"]}]
    ex = extract_gx(ev)
    assert ex.answer == "" and any("boom" in n for n in ex.notes)
    assert extract_gx([]).notes == ["no result line"]


def test_opencode_text_of_the_last_finished_step():
    ev = read_ndjson(S / "opencode-smoke-fix.jsonl")
    ex = extract_opencode(ev)
    # The earlier step's text ("The bug is at inventory.py:23 ...") is not the answer.
    assert ex.answer == (
        "Fixed: `total()` was looping over `range(len(lines) - 1)`, skipping the final line. It now iterates over "
        "all lines. All 3 tests pass."
    )
    assert "finish=stop" in ex.notes


def test_codex_output_file_then_fallback():
    ev = read_ndjson(S / "codex-smoke-fix.jsonl")
    last = (S / "codex-smoke-fix.last-message.txt").read_text()
    ex = extract_codex(ev, last)
    assert ex.answer == last.strip() and "4 passed" in ex.answer
    assert ex.extra["items"]["command_execution"] >= 1
    fb = extract_codex(ev, None)  # no -o file (a failed turn): the last agent_message
    msgs = [e["item"]["text"] for e in ev if e.get("type") == "item.completed" and e["item"].get("type") == "agent_message"]
    assert fb.answer == msgs[-1].strip()
    assert any("no -o file" in n for n in fb.notes)


def test_samples_hold_no_credentials():
    for p in S.iterdir():
        text = p.read_text()
        assert "craze-eval-dummy-key" not in text
        for line in text.splitlines():
            if line.startswith("{"):
                json.loads(line)


def test_recorded_samples_report_explicit_completion():
    """Review r1-c1 finding 17: each real sample ended in an explicit success."""
    assert extract_craze(read_ndjson(S / "craze-smoke-fix.jsonl")).completed
    assert extract_gx(read_ndjson(S / "gx-smoke-fix.jsonl")).completed
    assert extract_opencode(read_ndjson(S / "opencode-smoke-fix.jsonl")).completed
    assert extract_codex(read_ndjson(S / "codex-smoke-fix.jsonl"), "x").completed


def test_a_symlinked_last_message_or_plan_is_never_read(tmp_path):
    """Review r1-c1 finding 1: the agent owns its home; a last-message or plan file
    that is a symlink to an owner file (or a FIFO) is not followed on the host."""
    from crazeeval.runners.base import newest_text
    from crazeeval.runners.codex import CodexRunner
    from crazeeval.tasks import load_tasks

    secret = tmp_path / "owner" / "providers.toml"
    secret.parent.mkdir()
    secret.write_text('api_key = "sk-FAKE-owner-9876543210"\n')
    home = tmp_path / "home"
    home.mkdir()
    (home / "last-message.txt").symlink_to(secret)
    stdout = tmp_path / "stdout.jsonl"
    stdout.write_text('{"type": "item.completed", "item": {"type": "agent_message", "text": "the real answer"}}\n'
                      '{"type": "turn.completed", "usage": {}}\n')
    ex = CodexRunner().extract(stdout, home, load_tasks()["smoke-explain"])
    assert "sk-FAKE" not in ex.answer and ex.answer == "the real answer"
    # A plan directory reached through a planted link, and a plan that is a FIFO.
    (home / ".craze").symlink_to(secret.parent)
    assert newest_text(home, ".craze", "*.toml") is None
    (home / ".grok" / "sessions" / "s1").mkdir(parents=True)
    import os

    os.mkfifo(home / ".grok" / "sessions" / "s1" / "plan.md")
    assert newest_text(home, ".grok/sessions", "plan.md") is None
    (home / ".grok" / "sessions" / "s2").mkdir()
    (home / ".grok" / "sessions" / "s2" / "plan.md").write_text("# the plan\n")
    assert newest_text(home, ".grok/sessions", "plan.md") == "# the plan\n"
