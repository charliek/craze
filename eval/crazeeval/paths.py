"""Well-known locations. Everything the owner owns is a read-only input."""

from __future__ import annotations

import os
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = EVAL_DIR.parent
TASKS_DIR = EVAL_DIR / "tasks"
FIXTURES_DIR = EVAL_DIR / "fixtures"
PRICES_FILE = EVAL_DIR / "prices.toml"
MODELS_FILE = EVAL_DIR / "models.toml"

HOME = Path.home()
PLAN_DIR = Path(
    os.environ.get("CRAZEEVAL_PLAN_DIR", HOME / ".claude/plans/craze/029-native-harness-quality")
)
DEFAULT_LEDGER = PLAN_DIR / "ledger.jsonl"
DEFAULT_RUNS_DIR = PLAN_DIR / "eval-runs"
DEFAULT_CONFIG_ROOT = PLAN_DIR / "eval-config"
CACHE_DIR = Path(os.environ.get("CRAZEEVAL_CACHE_DIR", HOME / ".cache/crazeeval"))

# The owner's files: read-only inputs, never modified.
OWNER_CRAZE_NATIVE = HOME / ".craze/native"
OWNER_GROK = HOME / ".grok"

# craze at the plan's base commit (plan 029 §3.1.5).
CRAZE_TEMPLATE_COMMIT = "3eabb316bb8032893b3f66329f649b33950c1a73"

# The only credential a harness ever holds (plan 029 §3.1.2).
DUMMY_KEY = "craze-eval-dummy-key"
DUMMY_KEY_VAR = "CRAZE_EVAL_DUMMY"

# Where everything sits inside the sandbox. Nothing of /home exists in there.
SANDBOX_ROOT = "/sandbox"
SANDBOX_HOME = "/sandbox/home"
SANDBOX_WORK = "/sandbox/work"
SANDBOX_TOOLS = "/sandbox/tools"
SANDBOX_CONFIG = "/sandbox/config"
SANDBOX_GOMODCACHE = "/sandbox/gomodcache"
SANDBOX_GOCACHE = "/sandbox/gocache"
