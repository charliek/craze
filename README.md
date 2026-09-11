# craze

A Linux terminal UI for [Cursor CLI](https://cursor.com/cli), talking to
`cursor-agent acp` over ACP. You own the chrome; Cursor still runs the agent.

## Requirements

- Linux
- Go 1.24+ (this repo pins 1.24 via `.mise.toml`; `mise install`)
- A working Cursor login: `agent login`

## Build

```shell
make build
./bin/craze version
./bin/craze prompt --json --agent-bin ./bin/craze-fake-agent "hello"
```

Headless tests (after `make build`): `make test-cli`.

## Status

POC on Linux: `craze` opens a TUI; `craze prompt --json` is the headless path.
Tests use `craze-fake-agent` (no Cursor credentials in CI). Shed-lane integration
is explicitly later work.
