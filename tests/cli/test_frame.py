"""`craze frame` through the binary — plan 004 §3.15 layer 2, the CLI half.

The Go tests call `RunFrameScript` in process, which proves the model but not
the command: flag parsing, the `--fake-script` hand-off to the child, the
frame actually reaching stdout, and the exit codes. This suite runs the same
scripts the goldens cover through `./bin/craze frame` and checks that path.

It does not duplicate the goldens. The goldens own "every cell is exactly
right"; this owns "the command produced a frame of the right shape with the
right things in it, and exited the way it says it does".
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

# `craze frame` has no --workspace: it runs in the current directory, and the
# status row shows that directory's basename. Every run gets the same one so
# nothing here depends on pytest's tmp dir naming.
WORKDIR = "ws"


def frame(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    *,
    script: str,
    cols: int,
    rows: int,
    keys: str,
    theme: str = "craze-dark",
    no_force: bool = False,
    ansi: bool = False,
    timeout: str = "15s",
    provider: str | None = None,
    freeze: bool = False,
    step: str | None = None,
) -> subprocess.CompletedProcess[str]:
    work = tmp_path / WORKDIR
    work.mkdir(exist_ok=True)
    argv = [
        str(craze_bin),
        "frame",
        "--cols",
        str(cols),
        "--rows",
        str(rows),
        "--agent-bin",
        str(fake_agent_bin),
        "--fake-script",
        script,
        "--keys",
        keys,
        "--theme",
        theme,
        "--timeout",
        timeout,
    ]
    if no_force:
        argv.append("--no-force")
    if ansi:
        argv.append("--ansi")
    if provider:
        argv.extend(["--provider", provider])
    if freeze:
        argv.append("--freeze")
    # RunFrameScript isolates HOME itself; this keeps the child agent and the
    # skills scan out of the developer's home too, the same way the PTY suite
    # does, and drops the env vars that would override the flags under test.
    env = os.environ.copy()
    env["HOME"] = str(tmp_path)
    env.pop("CRAZE_AGENT_BIN", None)
    env.pop("CRAZE_FAKE_SCRIPT", None)
    env.pop("CRAZE_CONFIG", None)
    env.pop("CRAZE_PROVIDER", None)
    if step:
        env["CRAZE_FAKE_STEP"] = step
    return subprocess.run(
        argv, capture_output=True, text=True, cwd=str(work), env=env, timeout=120
    )


def frame_lines(proc: subprocess.CompletedProcess[str], cols: int, rows: int) -> list[str]:
    """The printed frame, with the height and width contract asserted on it."""
    assert proc.returncode == 0, f"exit {proc.returncode}: {proc.stderr[-3000:]}"
    lines = proc.stdout.split("\n")
    assert lines and lines[-1] == "", "the frame should end with one newline"
    lines = lines[:-1]
    assert len(lines) == rows, f"frame is {len(lines)} rows, want {rows}:\n{proc.stdout}"
    widest = max((len(ln) for ln in lines), default=0)
    assert widest <= cols, f"a line is {widest} cells wide, want <= {cols}"
    return lines


# script, cols, rows, keys, substrings that must be on the frame
CASES = [
    ("echo", 80, 24, "<wait:idle>go<enter><wait:text:echo: go>", ["❯ go", "echo: go"]),
    ("title", 80, 24, "<wait:idle>go<enter><wait:text:echo: go>", ["echo: go"]),
    (
        "markdown",
        120,
        40,
        "<wait:idle>go<enter><wait:text:Inline><wait:idle>",
        ["+ Thought", "Heading", "• first item", "  │ func main() {"],
    ),
    (
        "diff",
        80,
        24,
        "<wait:idle>go<enter><wait:text:done diff><wait:idle>",
        ["✓ read  main.go", "✓ edit  main.go  +1 −1", 'fmt.Println("hello, world")'],
    ),
    (
        "bigdiff",
        80,
        24,
        "<wait:idle>go<enter><wait:text:done bigdiff><wait:idle>",
        ["diff too large"],
    ),
    (
        "bash",
        80,
        24,
        "<wait:idle>go<enter><wait:text:done bash><wait:idle>",
        ["✓ bash  go vet ./...  exit 127", "Command 'go' not found"],
    ),
    (
        "task",
        80,
        24,
        "<wait:idle>go<enter><wait:text:● agent  Count main.go lines  running>"
        "<wait:text:done task><wait:idle>",
        ["✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast"],
    ),
    (
        "task-late",
        80,
        24,
        "<wait:idle>go<enter><wait:text:● agent  Count main.go lines  running>"
        "<wait:text:done task><wait:idle>",
        ["✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast"],
    ),
    (
        "tasks",
        80,
        24,
        "<wait:idle>go<enter><wait:text:done tasks><wait:idle>",
        ["✓ agent  Subagent research", "✓ bash  echo hi"],
    ),
    (
        "todos",
        100,
        30,
        "<wait:idle>go<enter><wait:text:done todos><wait:idle>",
        ["tasks: 3 planned", "TASKS 1/3", "┃ ▸ Edit main.go"],
    ),
    (
        "todos-notify",
        100,
        30,
        "<wait:idle>go<enter><wait:text:done todos><wait:idle>",
        ["TASKS 1/3"],
    ),
]


@pytest.mark.parametrize(
    "script,cols,rows,keys,wants", CASES, ids=[c[0] for c in CASES]
)
def test_frame_scripts(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    script: str,
    cols: int,
    rows: int,
    keys: str,
    wants: list[str],
) -> None:
    proc = frame(
        craze_bin, fake_agent_bin, tmp_path, script=script, cols=cols, rows=rows, keys=keys
    )
    text = "\n".join(frame_lines(proc, cols, rows))
    for want in wants:
        assert want in text, f"missing {want!r}:\n{text}"
    # The status rows are the bottom of every frame, and the workspace basename
    # is this suite's fixed directory rather than whatever pytest called it.
    assert f"{WORKDIR} │ cursor" in text, text
    # 005 §3.2 put the mode chip at the front of status row 2. Every script here
    # runs in the mode the fake starts a session in, so the chip is the same one
    # on all of them.
    assert "◆ agent" in text, text


def test_frame_hides_the_todo_writer(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="todos",
        cols=100,
        rows=30,
        keys="<wait:idle>go<enter><wait:text:done todos><wait:idle>",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "Update TODOs" not in text, text


def test_frame_question_card_answers(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="ask",
        cols=100,
        rows=30,
        keys="<wait:idle>go<enter><wait:card>1<space>3<enter><wait:text:asked:><wait:idle>",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "asked:answered:q1=opt-a;q2=opt-x,opt-z" in text, text


def test_frame_plan_card_accepts(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="plan",
        cols=100,
        rows=30,
        keys="<wait:idle>go<enter><wait:card>a<wait:text:planned:><wait:idle>",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "PLAN Fake Plan" in text, text
    assert "planned:accepted" in text, text


# --------------------------------------------------------------- 005 (cut two)
#
# The tokens below (`<tab>`, `<click:>`, `<press:>`, `<motion:>`, `<release:>`,
# `<drag:>`, `<wait:copied>`) exist in internal/tui/frame.go; what these cases
# own is that they survive the trip through the command's --keys flag, and that
# what cut two added is on the frame the command prints.

# PLAN_OFFERED drives the planmode script to the point where the offer stands.
# The fake starts that script in plan mode. Waiting on the placeholder rather
# than on <wait:idle> is deliberate: the offer needs both of the turn's endings
# and those two race, so what the composer draws is the proof both landed.
PLAN_OFFERED = (
    "<wait:idle>plan it<enter>"
    "<wait:text:planned: plan it><wait:text:enter implements this plan>"
)


def test_frame_plan_mode_offers_then_implements(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """§3.3 through the command: the offer, then Enter on an empty composer.

    The fake answers "implementing" only when session/set_mode reached it before
    session/prompt, so "WRONG ORDER" on the frame is craze having raced its own
    chain rather than chaining it.
    """
    offered = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="planmode",
        cols=100,
        rows=30,
        keys=PLAN_OFFERED,
    )
    text = "\n".join(frame_lines(offered, 100, 30))
    assert "◆ plan" in text, text
    assert "enter implements this plan  ·  type to refine" in text, text

    implemented = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="planmode",
        cols=100,
        rows=30,
        keys=PLAN_OFFERED + "<enter><wait:text:implementing:><wait:idle>",
    )
    text = "\n".join(frame_lines(implemented, 100, 30))
    assert "WRONG ORDER" not in text, text
    for want in ("mode → agent", "❯ Implement the plan above.", "◆ agent"):
        assert want in text, f"missing {want!r}:\n{text}"
    assert "implementing: Implement the plan above." in text, text


def test_frame_plan_offer_survives_the_card_cursor_sends(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """`planmode-card` is the plan turn a real cursor-agent sends.

    The card arrives before the events that arm the offer, so it used to retire
    the offer outright and a live plan-mode turn could never make one. What this
    pins is the wire-level shape: nothing offered while the card is up, the
    placeholder once it has been answered, and Enter then sending the implement
    prompt. The middle frame is the one that fails without the fix.
    """
    carded = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="planmode-card",
        cols=100,
        rows=30,
        keys="<wait:idle>plan it<enter><wait:card>",
    )
    text = "\n".join(frame_lines(carded, 100, 30))
    for want in ("drafting: plan it", "PLAN Print current time", "[a]ccept", "◆ plan"):
        assert want in text, f"missing {want!r}:\n{text}"
    # Nothing is offered yet, because the fake is blocked on the answer: the turn
    # has not ended, and a card is not a turn ending. The other half of the rule
    # — an offer already armed when a card opens — needs the card to land between
    # the turn's two endings, which a blocking cursor/create_plan cannot be
    # scripted into; internal/tui's own card/offer tests own that gap.
    assert "enter implements this plan" not in text, text

    answered = "<wait:idle>plan it<enter><wait:card>a<wait:text:planned: plan it>"
    offered = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="planmode-card",
        cols=100,
        rows=30,
        keys=answered + "<wait:text:enter implements this plan>",
    )
    text = "\n".join(frame_lines(offered, 100, 30))
    assert "plan Print current time → accepted" in text, text
    assert "enter implements this plan  ·  type to refine" in text, text

    implemented = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="planmode-card",
        cols=100,
        rows=30,
        keys=answered
        + "<wait:text:enter implements this plan><enter><wait:text:implementing:><wait:idle>",
    )
    text = "\n".join(frame_lines(implemented, 100, 30))
    assert "WRONG ORDER" not in text, text
    for want in (
        "mode → agent",
        "❯ Implement the plan above.",
        "implementing: Implement the plan above.",
        "◆ agent",
    ):
        assert want in text, f"missing {want!r}:\n{text}"


def test_frame_model_dialog_turns_fast_on(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """§3.4 through the command: /model opens the dialog and Enter applies fast.

    Tab twice reaches the fast row and one right arms it; the note says what was
    applied and status row 1 says it came back, which together are the proof the
    value went out to the agent rather than only into the dialog's own state.
    """
    opened = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        cols=100,
        rows=30,
        keys="<wait:idle>/model<enter>",
    )
    text = "\n".join(frame_lines(opened, 100, 30))
    for want in ("> Default", "current", "effort  low  [medium]  high", "fast  [off]  on"):
        assert want in text, f"missing {want!r}:\n{text}"

    applied = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        cols=100,
        rows=30,
        keys="<wait:idle>/model<enter><tab><tab><right><enter><wait:text:fast → on>",
    )
    text = "\n".join(frame_lines(applied, 100, 30))
    assert "fast → on" in text, text
    assert "Default (medium · fast)" in text, text
    # The dialog is a layer over the transcript, and applying closes it.
    assert "type to filter" not in text, text


def test_frame_mode_chip_click_cycles(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The chip is clickable, not just drawn: a press on it cycles the mode.

    Status row 2 is the last row of the frame, so its y is rows-1, and the chip
    is the first thing on it — x 1 is inside `◆ agent`.
    """
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        cols=80,
        rows=24,
        keys="<wait:idle><click:1,23><wait:text:mode →>",
    )
    text = "\n".join(frame_lines(proc, 80, 24))
    assert "mode → plan" in text, text
    assert "◆ plan" in text, text
    assert "◆ agent" not in text, text


def test_frame_drag_over_the_reply_copies_it(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """§3.5 through the command: a drag over two transcript rows copies them.

    Rows 0 and 1 of a finished `echo` turn are the user line and the reply, so a
    drag from the first cell to the middle of the second row covers exactly two
    rows. The same gesture is sent twice — once as one `<drag:>` and once as the
    three reports it stands for — because `<release:>` deliberately reports no
    button, the way an X10 terminal does, and the two spellings have to land on
    the same frame.
    """
    said = "<wait:idle>go<enter><wait:text:echo: go><wait:idle>"

    def select(gesture: str) -> list[str]:
        proc = frame(
            craze_bin,
            fake_agent_bin,
            tmp_path,
            script="echo",
            cols=100,
            rows=30,
            keys=said + gesture + "<wait:copied>",
        )
        lines = frame_lines(proc, 100, 30)
        text = "\n".join(lines)
        assert "copied 2 lines" in text, text
        # Nothing may reach the developer's clipboard, and an OSC 52 sequence in
        # this stream would corrupt the frame: the runner records both writes.
        assert "\x1b]52" not in proc.stdout, proc.stdout
        # Status row 1 carries the session clock, so it is the one row two runs
        # are allowed to differ on.
        return [ln for ln in lines if f"{WORKDIR} │ cursor" not in ln]

    dragged = select("<drag:0,0,7,1>")
    reports = select("<press:0,0><motion:7,1><release:7,1>")
    assert reports == dragged, (
        "press/motion/release did not draw the same frame as <drag:>\n"
        + "\n".join(reports)
        + "\n--- drag ---\n"
        + "\n".join(dragged)
    )


def test_frame_permission_line_needs_no_force(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="permission",
        cols=100,
        rows=30,
        keys="<wait:idle>go<enter><wait:card>",
        no_force=True,
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "permission Shell  [a]llow once  [A]lways  [n] reject" in text, text
    assert "▸ prompting for permissions" in text, text


def test_frame_cancel_says_so(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="hang",
        cols=80,
        rows=24,
        keys="<wait:idle>hang<enter><wait:working><esc><wait:text:cancelled>",
    )
    text = "\n".join(frame_lines(proc, 80, 24))
    assert "cancelled" in text, text


def test_frame_too_small(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = frame(
        craze_bin, fake_agent_bin, tmp_path, script="echo", cols=30, rows=8, keys="<wait:idle>"
    )
    text = "\n".join(frame_lines(proc, 30, 8))
    assert "terminal too small" in text, text


def test_frame_theme_flag_changes_the_palette(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """--ansi is the only way to see the palette, and the flag has to reach it."""
    dark = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        cols=80,
        rows=24,
        keys="<wait:idle>",
        ansi=True,
    )
    gruvbox = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        cols=80,
        rows=24,
        keys="<wait:idle>",
        theme="gruvbox",
        ansi=True,
    )
    assert dark.returncode == 0 and gruvbox.returncode == 0
    assert "\x1b[" in dark.stdout, "--ansi should force a colour profile"
    assert dark.stdout != gruvbox.stdout, "--theme did not reach the frame"


def test_frame_bad_token_exits_2(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin, fake_agent_bin, tmp_path, script="echo", cols=80, rows=24, keys="<nope>"
    )
    assert proc.returncode == 2, proc.stdout + proc.stderr
    assert "unknown token" in proc.stderr, proc.stderr


def test_frame_wait_timeout_exits_3_with_the_last_frame(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="hang",
        cols=80,
        rows=24,
        keys="<wait:idle>hang<enter><wait:idle>",
        timeout="2s",
    )
    assert proc.returncode == 3, proc.stdout + proc.stderr
    assert "<wait:idle>" in proc.stderr, proc.stderr
    assert "last frame:" in proc.stderr, proc.stderr
    assert "esc to interrupt" in proc.stderr, proc.stderr


def test_frame_ignores_craze_provider_env(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The frame runner is hermetic: $CRAZE_PROVIDER must not change goldens."""
    work = tmp_path / WORKDIR
    work.mkdir(exist_ok=True)
    env = os.environ.copy()
    env["HOME"] = str(tmp_path)
    env["CRAZE_PROVIDER"] = "grok"
    env.pop("CRAZE_AGENT_BIN", None)
    env.pop("CRAZE_FAKE_SCRIPT", None)
    env.pop("CRAZE_CONFIG", None)
    proc = subprocess.run(
        [
            str(craze_bin),
            "frame",
            "--cols",
            "80",
            "--rows",
            "24",
            "--agent-bin",
            str(fake_agent_bin),
            "--fake-script",
            "echo",
            "--keys",
            "<wait:idle>",
            "--theme",
            "craze-dark",
            "--timeout",
            "15s",
        ],
        capture_output=True,
        text=True,
        cwd=str(work),
        env=env,
        timeout=120,
    )
    text = "\n".join(frame_lines(proc, 80, 24))
    assert f"{WORKDIR} │ cursor" in text, text
    assert "starting…" not in text, text


def test_frame_grok_echo(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="grok-echo",
        cols=80,
        rows=24,
        keys="<wait:idle>go<enter><wait:text:echo: go><wait:idle>",
        provider="grok",
    )
    text = "\n".join(frame_lines(proc, 80, 24))
    assert f"{WORKDIR} │ grok" in text, text
    assert "echo: go" in text, text
    assert "fast" not in text, text
    assert "◆ default" in text, text


# --------------------------------------------------------------- 007 §3.9
#
# The grok sub-agent scripts need --provider grok the same way
# test_frame_grok_echo does: the cursor dialect drops every
# _x.ai/session_notification, so without the flag there is no row to wait on.
# The keys mirror internal/tui/frame_test.go's goldens, which own the exact
# cells; these own the trip through the command. The rows case waits on the
# progress suffix (`4.7k tok`) rather than on the row itself because the
# suffix is what proves the progress notification landed, the way the Go
# golden's <wait:text:4.7k tok> does.

# case id, script, cols, rows, keys, substrings that must be on the frame
GROK_SUBAGENT_CASES = [
    (
        "grok-subagent-rows",
        "grok-subagent",
        80,
        24,
        "<wait:idle>go<enter><wait:text:4.7k tok>",
        ["○ explore  List directory files  0s · 4.7k tok", "← 1 agent"],
    ),
    (
        "grok-subagent-view",
        "grok-subagent",
        80,
        24,
        "<wait:idle>go<enter><wait:text:4.7k tok><down><enter><wait:text:esc to return>",
        [
            "(grok-4.6) List directory files",
            "read-only · esc to return",
            "✓ tool  list_dir",
            "● explore",
            "list_dir · 0s · 4.7k tok",
        ],
    ),
    (
        "grok-subagent-two",
        "grok-subagent-two",
        100,
        30,
        "<wait:idle>go<enter><wait:text:4.7k tok><down><enter><tab><wait:text:✓ tool  read_file>",
        ["Report README first line", "esc to return · tab next agent", "← 2 agents"],
    ),
    (
        "grok-subagent-fail",
        "grok-subagent-fail",
        80,
        24,
        "<wait:idle>go<enter><wait:text:✗ explore><wait:idle>",
        ["✗ explore  List directory files  2.9s · grok-4.6"],
    ),
    (
        "grok-subagent-cancel",
        "grok-subagent-cancel",
        100,
        30,
        "<wait:idle>go<enter><wait:text:○ general-purpose><esc>"
        "<wait:text:– general-purpose><down><enter><wait:text:Execute sleep>",
        ["– tool  Execute sleep 45 && echo finished", "cancelled"],
    ),
    (
        "grok-subagent-late",
        "grok-subagent-late",
        80,
        24,
        "<wait:idle>go<enter><wait:idle>",
        ["○ explore", "← 1 agent"],
    ),
]


@pytest.mark.parametrize(
    "case,script,cols,rows,keys,wants",
    GROK_SUBAGENT_CASES,
    ids=[c[0] for c in GROK_SUBAGENT_CASES],
)
def test_frame_grok_subagent_scripts(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    case: str,
    script: str,
    cols: int,
    rows: int,
    keys: str,
    wants: list[str],
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script=script,
        cols=cols,
        rows=rows,
        keys=keys,
        provider="grok",
    )
    text = "\n".join(frame_lines(proc, cols, rows))
    for want in wants:
        assert want in text, f"missing {want!r}:\n{text}"
    # The grok status row names the provider and the default mode chip, the
    # same two probes test_frame_grok_echo pins.
    assert f"{WORKDIR} │ grok" in text, text
    assert "◆ default" in text, text


def test_frame_task_view_receipt(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The cursor sub-agent view: same chrome, receipt-only content.

    `task` runs under the default cursor provider. The row lingers after the
    turn ends, so `<down><enter>` still opens it once `done task` has been
    waited on — the view is the receipt (note, prompt, model line), never a
    transcript cursor never streamed.
    """
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="task",
        cols=100,
        rows=30,
        keys="<wait:idle>go<enter><wait:text:done task><wait:idle><down><enter>",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    for want in (
        "✓ @task · completed · receipt only · esc to return",
        "(grok-4.6-high-fast) Count main.go lines",
        "streams no sub-agent transcript",
        "● task  Count main.go lines",
    ):
        assert want in text, f"missing {want!r}:\n{text}"


# ---------------------------------------------------------------- queued messages

# The long turn runs two tool steps; a step long enough to type into is what
# makes these frames the same on any machine, and --freeze stops the clock so
# the elapsed counter and the spinner glyph do not move either.
QUEUE_TWO = (
    "<wait:idle>go the long way<enter><wait:working>"
    "Reply with PINEAPPLE<enter>Reply with MANGO<enter>"
)


def test_frame_queue_band(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=100,
        rows=30,
        keys=QUEUE_TWO + "<wait:text:#2 Reply with MANGO>",
        freeze=True,
        step="30s",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "#1 Reply with PINEAPPLE" in text, text
    assert "#2 Reply with MANGO" in text, text
    assert "⧗ 2 queued" in text, text
    assert "enter queues  ·  ctrl+l sends now" in text, text
    # A queued message is not in the transcript until it is sent.
    assert "❯ Reply with PINEAPPLE" not in text, text


def test_frame_queue_hover_shows_the_actions(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=100,
        rows=30,
        keys=QUEUE_TWO + "<wait:text:#2 Reply with MANGO><hover:10,22>",
        freeze=True,
        step="30s",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "[send now] [edit] [cancel]" in text, text


def test_frame_queue_up_selects_and_enter_edits(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=100,
        rows=30,
        keys=QUEUE_TWO + "<wait:text:#2 Reply with MANGO><up><enter>",
        freeze=True,
        step="30s",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "editing #2" in text, text
    assert "❯ Reply with MANGO" in text, text
    assert "#2 Reply with MANGO" in text, text


def test_frame_queue_backspace_cancels_a_row(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=100,
        rows=30,
        keys=QUEUE_TWO + "<wait:text:#2 Reply with MANGO><up><backspace><wait:gone:MANGO>",
        freeze=True,
        step="30s",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "#1 Reply with PINEAPPLE" in text, text
    assert "MANGO" not in text, text
    assert "⧗ 1 queued" in text, text


def test_frame_send_now_asks_first_on_cursor(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=100,
        rows=30,
        keys="<wait:idle>go the long way<enter><wait:working>Reply with PINEAPPLE<ctrl-l>",
        freeze=True,
        step="30s",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "cancel the running turn and send? enter · esc" in text, text


def test_frame_grok_interject_joins_the_turn(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="grok-long-turn",
        cols=100,
        rows=30,
        keys=(
            "<wait:idle>go the long way<enter><wait:working>Also say BANANA<ctrl-l>"
            "<wait:text:DONE step1 Also say BANANA step2><wait:idle>"
        ),
        provider="grok",
        freeze=True,
        step="2s,1ms",
    )
    text = "\n".join(frame_lines(proc, 100, 30))
    assert "↳ Also say BANANA" in text, text
    assert "DONE step1 Also say BANANA step2" in text, text
    # An interjection never cancels the turn it joined.
    assert "cancelled" not in text, text


def test_frame_queue_drains_in_order(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = frame(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        cols=80,
        rows=24,
        keys=(
            QUEUE_TWO
            + "<wait:text:#2 Reply with MANGO><wait:text:❯ Reply with MANGO><wait:idle>"
        ),
        freeze=True,
        step="2s,1ms",
    )
    text = "\n".join(frame_lines(proc, 80, 24))
    assert text.index("❯ Reply with PINEAPPLE") < text.index("❯ Reply with MANGO"), text
    assert "#1 Reply" not in text, text
    assert "⧗ " not in text, text
