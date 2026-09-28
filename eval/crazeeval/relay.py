"""The in-sandbox relay (review r1-c1 finding 12).

An agent run has its own network namespace with no route out. This relay -- run by
the sandbox's own python, stdlib only -- listens on ``127.0.0.1:<port>`` inside that
namespace and pipes each connection to the proxy's Unix socket, bound into the
sandbox. It then runs the harness as its child and exits with the child's status, so
the harness's base URLs stay ``http://127.0.0.1:<port>/r/…`` and the proxy is the
only thing it can reach.

usage: python -I -S relay.py --port N --unix /sandbox/proxy.sock -- <harness argv...>
"""

import asyncio
import sys


async def _pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
        if writer.can_write_eof():
            writer.write_eof()
    except (OSError, asyncio.CancelledError):
        pass


async def _serve(client_r, client_w, unix_path: str) -> None:
    try:
        up_r, up_w = await asyncio.open_unix_connection(unix_path)
    except OSError:
        client_w.close()
        return
    try:
        await asyncio.gather(_pipe(client_r, up_w), _pipe(up_r, client_w))
    finally:
        for w in (up_w, client_w):
            try:
                w.close()
            except OSError:
                pass


async def main(port: int, unix_path: str, cmd: list[str]) -> int:
    server = await asyncio.start_server(lambda r, w: _serve(r, w, unix_path), "127.0.0.1", port)
    try:
        proc = await asyncio.create_subprocess_exec(*cmd, stdin=asyncio.subprocess.DEVNULL)
    except OSError as e:
        print(f"crazeeval relay: cannot start {cmd[0]!r}: {e}", file=sys.stderr)
        server.close()
        return 127
    rc = await proc.wait()
    server.close()
    return rc if rc >= 0 else 128 - rc


def parse(argv: list[str]) -> tuple[int, str, list[str]]:
    if "--" not in argv:
        raise SystemExit("usage: relay.py --port N --unix PATH -- cmd...")
    i = argv.index("--")
    opts, cmd = argv[:i], argv[i + 1 :]
    port = int(opts[opts.index("--port") + 1])
    unix_path = opts[opts.index("--unix") + 1]
    if not cmd:
        raise SystemExit("relay.py: no command")
    return port, unix_path, cmd


if __name__ == "__main__":
    p, u, c = parse(sys.argv[1:])
    sys.exit(asyncio.run(main(p, u, c)))
