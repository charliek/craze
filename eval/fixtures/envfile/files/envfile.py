"""Read `.env` files: KEY=value lines, as the deploy scripts write them.

Supported:

- blank lines and lines starting with ``#`` are ignored;
- an optional ``export `` prefix;
- values may be wrapped in single or double quotes, which are removed;
  inside double quotes, ``\\n`` becomes a newline and ``\\"`` a quote;
- an unquoted value ends at an inline comment (`` # ...``);
- whitespace around the key, the ``=`` and an unquoted value is dropped.
"""

from __future__ import annotations

import re

KEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


class ParseError(ValueError):
    def __init__(self, lineno: int, message: str):
        super().__init__(f"line {lineno}: {message}")
        self.lineno = lineno


def _unquote(value: str, lineno: int) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        inner = value[1:-1]
        if value[0] == '"':
            inner = inner.replace("\\n", "\n").replace('\\"', '"')
        return inner
    if value[:1] in ("'", '"'):
        raise ParseError(lineno, "unterminated quoted value")
    return value


def parse(text: str) -> dict[str, str]:
    """Parse the text of a .env file into a dict (later keys win)."""
    out: dict[str, str] = {}
    for lineno, raw in enumerate(text.splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export ") :].lstrip()
        key, sep, value = line.partition("=")
        if not sep:
            raise ParseError(lineno, f"expected KEY=value, got {raw!r}")
        key = key.strip()
        if not KEY_RE.match(key):
            raise ParseError(lineno, f"invalid key {key!r}")
        # Drop an inline comment.
        if "#" in value:
            value = value[: value.index("#")]
        out[key] = _unquote(value.strip(), lineno)
    return out


def load(path: str) -> dict[str, str]:
    with open(path, encoding="utf-8") as f:
        return parse(f.read())
