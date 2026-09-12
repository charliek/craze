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
    # RunFrameScript isolates HOME itself; this keeps the child agent and the
    # skills scan out of the developer's home too, the same way the PTY suite
    # does, and drops the env vars that would override the flags under test.
    env = os.environ.copy()
    env["HOME"] = str(tmp_path)
    env.pop("CRAZE_AGENT_BIN", None)
    env.pop("CRAZE_FAKE_SCRIPT", None)
    env.pop("CRAZE_CONFIG", None)
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
