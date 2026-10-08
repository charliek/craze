"""pid_alive, the liveness check of the host cleanup and of the tests.

A zombie still answers signal 0, so pid_alive also reads /proc/<pid>/stat.
The process can be reaped between the two: CI met it at 659fa8c, where
test_hub's cleanup check read a hub reaped in that gap as alive. A /proc entry
that cannot be read is not proof either way (macOS has no /proc; hidepid or a
masked /proc can hide a live process), so pid_alive asks signal 0 again. The
first tests stand in for os.kill and for conftest's Path, so no real pid can be
reused under them; the last checks the real live and zombie states.
"""

from __future__ import annotations

import subprocess
import sys
import time

import pytest

import conftest


def _unreadable(err: OSError) -> type:
    """A stand-in for conftest.Path whose every read fails with err."""

    class Unreadable:
        def __init__(self, *args: object) -> None:
            pass

        def read_bytes(self) -> bytes:
            raise err

    return Unreadable


def _kill_answering(*answers: BaseException | None) -> tuple[list[int], object]:
    """An os.kill whose successive signal-0 calls answer as given (None: the
    pid answers), and the list of signals it was sent."""
    sent: list[int] = []
    queue = list(answers)

    def kill(pid: int, sig: int) -> None:
        sent.append(sig)
        answer = queue.pop(0)
        if answer is not None:
            raise answer

    return sent, kill


# Each test patches inside its own MonkeyPatch.context(), undone before the
# test returns: the autouse host_cleanup's teardown calls the real os.kill and
# reads the real /proc.


def test_a_zombie_reaped_after_signal_0_is_gone() -> None:
    sent, kill = _kill_answering(None, ProcessLookupError())
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(conftest.os, "kill", kill)
        mp.setattr(conftest, "Path", _unreadable(FileNotFoundError("reaped")))
        assert conftest.pid_alive(4242) is False
    assert sent == [0, 0]


@pytest.mark.parametrize(
    "err",
    [FileNotFoundError("no /proc"), PermissionError("hidden")],
    ids=["no-proc-entry", "proc-unreadable"],
)
def test_a_process_that_still_answers_is_alive(err: OSError) -> None:
    sent, kill = _kill_answering(None, None)
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(conftest.os, "kill", kill)
        mp.setattr(conftest, "Path", _unreadable(err))
        assert conftest.pid_alive(4242) is True
    assert sent == [0, 0]


def test_a_live_child_is_alive_and_its_zombie_is_not() -> None:
    child = subprocess.Popen(["sleep", "30"])
    try:
        assert conftest.pid_alive(child.pid)
        if sys.platform == "linux":
            child.kill()
            # Killed and not yet waited for, it is a zombie: signal 0 still
            # answers, and /proc says Z once the kernel has finished with it.
            # Unreaped, its pid cannot be reused.
            deadline = time.monotonic() + 5
            while conftest.pid_alive(child.pid) and time.monotonic() < deadline:
                time.sleep(0.01)
            assert not conftest.pid_alive(child.pid), conftest.describe_pid(child.pid)
    finally:
        child.kill()
        child.wait()
