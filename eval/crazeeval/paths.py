"""Well-known locations. Everything the owner owns is a read-only input.

**Campaigns.** A campaign is one directory holding a series of batches (``eval-runs/``),
the budget ledger (``ledger.jsonl``) and the config snapshots (``eval-config/``). It is
resolved, first match wins, from:

1. the global ``--campaign NAME`` option (:func:`set_campaign`): ``~/.craze-eval/<NAME>``;
2. ``CRAZEEVAL_CAMPAIGN_DIR``: a full path;
3. ``CRAZEEVAL_PLAN_DIR``: the same, under its old name (plan 029's scripts set it);
4. ``~/.craze-eval/default``.

Nothing defaults into a plan folder. The resolution is lazy -- every call reads the
environment and the home directory again -- so the module-level names below
(``PLAN_DIR``, ``DEFAULT_LEDGER``, ``DEFAULT_RUNS_DIR``, ``DEFAULT_CONFIG_ROOT``) are
computed on access (PEP 562), never frozen at import.
"""

from __future__ import annotations

import os
import re
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = EVAL_DIR.parent
TASKS_DIR = EVAL_DIR / "tasks"
FIXTURES_DIR = EVAL_DIR / "fixtures"
PRICES_FILE = EVAL_DIR / "prices.toml"
MODELS_FILE = EVAL_DIR / "models.toml"

HOME = Path.home()
CACHE_DIR = Path(os.environ.get("CRAZEEVAL_CACHE_DIR", HOME / ".cache/crazeeval"))

# The owner's files: read-only inputs, never modified.
OWNER_CRAZE_NATIVE = HOME / ".craze/native"
OWNER_GROK = HOME / ".grok"

# craze at the plan's base commit (plan 029 §3.1.5): the commit a craze-repo task
# materialises unless its task.toml pins another (``[repo] commit``).
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

# -- campaigns ------------------------------------------------------------------------------

CAMPAIGN_ENV = "CRAZEEVAL_CAMPAIGN_DIR"
LEGACY_CAMPAIGN_ENV = "CRAZEEVAL_PLAN_DIR"  # the old name, kept as an alias
DEFAULT_CAMPAIGN = "default"
CAMPAIGNS_DIRNAME = ".craze-eval"
RUNS_DIRNAME = "eval-runs"
CONFIG_DIRNAME = "eval-config"
LEDGER_NAME = "ledger.jsonl"
CAMPAIGN_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$")

_campaign_name: str | None = None  # set by the CLI's --campaign


class CampaignError(ValueError):
    pass


def campaigns_root() -> Path:
    """``~/.craze-eval``: where a named campaign lives (the home directory read now)."""
    return Path.home() / CAMPAIGNS_DIRNAME


def check_campaign_name(name: str) -> str:
    """A campaign name is one path component: letters, digits, ``.``, ``_``, ``-``."""
    if not isinstance(name, str) or not CAMPAIGN_NAME_RE.match(name) or name in (".", ".."):
        raise CampaignError(f"invalid campaign name {name!r}: use letters, digits, '.', '_' or '-' (no '/')")
    return name


def set_campaign(name: str | None) -> None:
    """Select ``~/.craze-eval/<name>`` for this process (the global ``--campaign``);
    ``None`` returns to the environment and the default."""
    global _campaign_name
    _campaign_name = None if name is None else check_campaign_name(name)


def campaign_source() -> str:
    """Which rule chose the campaign: ``--campaign``, the env var, its alias or the default."""
    if _campaign_name is not None:
        return "--campaign"
    if os.environ.get(CAMPAIGN_ENV):
        return CAMPAIGN_ENV
    if os.environ.get(LEGACY_CAMPAIGN_ENV):
        return LEGACY_CAMPAIGN_ENV
    return "default"


def campaign_dir() -> Path:
    """The current campaign's directory (see the module docstring for the order)."""
    if _campaign_name is not None:
        return campaigns_root() / _campaign_name
    for var in (CAMPAIGN_ENV, LEGACY_CAMPAIGN_ENV):
        v = os.environ.get(var)
        if v:
            return Path(v).expanduser()
    return campaigns_root() / DEFAULT_CAMPAIGN


def campaign_name(d: Path | None = None) -> str:
    """A campaign's name: its directory's last component."""
    return Path(d or campaign_dir()).name


def runs_dir() -> Path:
    """Where ``run`` puts a batch by default: ``<campaign>/eval-runs``."""
    return campaign_dir() / RUNS_DIRNAME


def ledger_path() -> Path:
    """The campaign's budget ledger: ``<campaign>/ledger.jsonl``."""
    return campaign_dir() / LEDGER_NAME


def config_root() -> Path:
    """The campaign's config snapshots: ``<campaign>/eval-config``."""
    return campaign_dir() / CONFIG_DIRNAME


_LAZY = {
    "PLAN_DIR": campaign_dir,  # the old name for the campaign directory
    "DEFAULT_LEDGER": ledger_path,
    "DEFAULT_RUNS_DIR": runs_dir,
    "DEFAULT_CONFIG_ROOT": config_root,
}


def __getattr__(name: str):
    """The old import-time constants, resolved on every access (PEP 562)."""
    fn = _LAZY.get(name)
    if fn is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    return fn()
