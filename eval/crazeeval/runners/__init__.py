"""Headless runners, one per harness."""

from crazeeval.runners.base import Extract, Runner
from crazeeval.runners.codex import CodexRunner
from crazeeval.runners.craze import CrazeRunner
from crazeeval.runners.gx import GxRunner
from crazeeval.runners.opencode import OpencodeRunner

RUNNERS: dict[str, Runner] = {
    "craze": CrazeRunner(),
    "gx": GxRunner(),
    "opencode": OpencodeRunner(),
    "codex": CodexRunner(),
}

__all__ = ["RUNNERS", "Runner", "Extract"]
