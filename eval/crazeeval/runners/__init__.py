"""Headless runners, one per harness."""

from crazeeval.runners.base import Extract, Runner
from crazeeval.runners.codex import CodexRunner
from crazeeval.runners.craze import CrazeRunner, CrazeWebRunner
from crazeeval.runners.opencode import OpencodeRunner

RUNNERS: dict[str, Runner] = {
    "craze": CrazeRunner(),
    "opencode": OpencodeRunner(),
    "codex": CodexRunner(),
    "craze-web": CrazeWebRunner(),
}

__all__ = ["RUNNERS", "Runner", "Extract"]
