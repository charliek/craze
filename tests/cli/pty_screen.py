"""Terminal cells for the CLI PTY tests, including Bubble Tea's xterm output."""

import re
from collections.abc import Callable

import pyte


class TerminalScreen(pyte.Screen):
    def __init__(self, cols: int, rows: int, record_screen: Callable[[], None]) -> None:
        super().__init__(cols, rows)
        self.record_screen = record_screen
        self.primary = None
        self.last_char = ""
        self.synchronized = False

    def draw(self, data: str) -> None:
        super().draw(data)
        if data:
            self.last_char = data[-1]

    def repeat_character(self, count: int = 1) -> None:
        # ECMA-48 REP repeats the preceding graphic character. The renderer
        # can use it for a long horizontal rule or a run of blanks.
        self.draw(self.last_char * (count or 1))

    def scroll_up(self, count: int = 1) -> None:
        self._scroll(count, self.delete_lines)

    def scroll_down(self, count: int = 1) -> None:
        self._scroll(count, self.insert_lines)

    def _scroll(self, count: int, shift_lines: Callable[[int], None]) -> None:
        # SU/SD scroll only the current margins and leave the cursor in place.
        x, y = self.cursor.x, self.cursor.y
        self.cursor.y = self.margins.top if self.margins else 0
        shift_lines(count or 1)
        self.cursor.x, self.cursor.y = x, y

    def set_mode(self, *modes: int, private: bool = False) -> None:
        if private and 2026 in modes:
            self.synchronized = True
        if private and 1049 in modes and self.primary is None:
            self.record_screen()
            self.primary = self.buffer, self.cursor
            blank = pyte.Screen(self.columns, self.lines)
            self.buffer, self.cursor = blank.buffer, blank.cursor
        super().set_mode(*modes, private=private)

    def reset_mode(self, *modes: int, private: bool = False) -> None:
        if private and 1049 in modes and self.primary is not None:
            self.record_screen()
            self.buffer, self.cursor = self.primary
            self.primary = None
        super().reset_mode(*modes, private=private)
        if private and 2026 in modes:
            self.synchronized = False
            self.record_screen()


class TerminalStream(pyte.ByteStream):
    csi = {**pyte.Stream.csi, "b": "repeat_character", "S": "scroll_up", "T": "scroll_down"}

    # These xterm and Kitty controls select keyboard encodings and leave
    # the screen alone. pyte treats the xterm command as SGR and displays
    # the tail of a Kitty pop as text. A control can span multiple PTY reads.
    _keyboard = re.compile(rb"\x1b\[(?:>4(?:;[0-9]+)?m|[<>][0-9;]*u)")
    _incomplete = re.compile(rb"\x1b(?:\[(?:[<>][0-9;]*)?)?\Z")

    def __init__(self, screen: TerminalScreen) -> None:
        super().__init__(screen)
        self.pending = b""

    def feed(self, data: bytes) -> None:
        data = self.pending + data
        partial = self._incomplete.search(data)
        end = partial.start() if partial else len(data)
        self.pending = data[end:]
        super().feed(self._keyboard.sub(b"", data[:end]))
