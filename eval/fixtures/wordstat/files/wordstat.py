"""wordstat: line, word and character counts for text files.

    $ python wordstat.py notes.md todo.txt
       lines    words    chars  path
          12       80      431  notes.md
           3        9       41  todo.txt
          15       89      472  total
"""

from __future__ import annotations

import argparse
import sys
from dataclasses import dataclass


@dataclass
class Counts:
    lines: int = 0
    words: int = 0
    chars: int = 0

    def __add__(self, other: "Counts") -> "Counts":
        return Counts(self.lines + other.lines, self.words + other.words, self.chars + other.chars)


def count_text(text: str) -> Counts:
    return Counts(lines=len(text.splitlines()), words=len(text.split()), chars=len(text))


def count_file(path: str) -> Counts:
    with open(path, encoding="utf-8") as f:
        return count_text(f.read())


def format_table(rows: list[tuple[str, Counts]], total: Counts | None) -> str:
    out = [f"{'lines':>8} {'words':>8} {'chars':>8}  path"]
    for path, c in rows:
        out.append(f"{c.lines:>8} {c.words:>8} {c.chars:>8}  {path}")
    if total is not None:
        out.append(f"{total.lines:>8} {total.words:>8} {total.chars:>8}  total")
    return "\n".join(out)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="wordstat", description="Count lines, words and characters.")
    p.add_argument("paths", nargs="+", help="files to count")
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    rows = []
    for path in args.paths:
        try:
            rows.append((path, count_file(path)))
        except OSError as e:
            print(f"wordstat: {path}: {e.strerror}", file=sys.stderr)
            return 1
    total = None
    if len(rows) > 1:
        total = sum((c for _, c in rows), Counts())
    print(format_table(rows, total))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
