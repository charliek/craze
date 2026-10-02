.PHONY: build lint test test-race test-cli docs docs-serve clean

# mise shims first so `make` works in a non-activated shell.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

VERSION ?= dev
LDFLAGS := -X github.com/charliek/craze/internal/version.Version=$(VERSION)

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/craze ./cmd/craze
	go build -o bin/craze-fake-agent ./cmd/craze-fake-agent
	go build -o bin/craze-fake-host ./cmd/craze-fake-host

# The go list check below is one half of craze's import boundary: internal/tui
# and internal/cli must not import internal/acp. The other half is the
# depguard rule in .golangci.yml, which golangci-lint runs: internal/harness
# must not import the rest of craze (D-02; plan 018 §3.1).
lint:
	golangci-lint run
	GOOS=darwin golangci-lint run
	@n=$$(go list -f '{{join .Imports "\n"}}' ./internal/tui ./internal/cli | grep -c internal/acp || true); \
	echo "$$n"; test "$$n" = "0"

# CRAZE_GOLDEN_TRANSPORT is pinned to both in the two test targets, whatever
# the caller exported: every frame golden runs in process AND over the control
# socket in the gate (plan 027 §3.16). A developer narrows the matrix — the
# local fast loop, CRAZE_GOLDEN_TRANSPORT=inproc — only by running go test
# directly.
test:
	CRAZE_GOLDEN_TRANSPORT=both go test -timeout 5m -v ./...

# The packages with concurrency worth the 10x slowdown: the ACP client's
# reader and writer goroutines, the session's event fan-out, the engine (one
# session's driver, its queue and its workers, raced from plan 021 on — the
# whole tree, so a future subpackage is raced as it lands), the TUI's
# clipboard seams and leaked command goroutines, the host Hub's per-reporter
# workers, the native harness (its turn runner persists from stream
# callbacks while cancel and Close run on other goroutines; the whole tree is
# named, so each harness package is raced as it lands), the session
# journal (one writer goroutine owns the file while Append, Note, Close and
# live readers arrive from others, and its tests stall that writer on
# purpose), the transcript model (the engine folds it inside the log's
# boundary while snapshots are cut from other goroutines; plan 024 §3.4), and
# the control-socket wire (plan 027 §5: each package the plan adds joins in
# the commit that creates it, ahead of the server and client that run its
# framing on every connection's goroutines), and the control-socket server
# (a reader, a writer and a handler per request on every connection, the
# binding table's transfers racing its releases, and Close racing all of it),
# and the runtime namespace (a host's registry rewrites racing its Close under
# one mutex, and flock contention between open file descriptions), and the
# TUI's backend seam (internal/backend: types today, joined in the commit that
# creates it as plan 027 §5 asks), and the CLI (plan 030 §5: from PR 1 on
# `craze serve` is a host whose lifecycle is goroutines — the stop
# coordinator, the idle watcher, the registry writer and the spawner's reaper
# race the server's close and each other — joined in PR 1's first code commit,
# ahead of them; about 27 s of -race here), and the session list's poller
# (plan 030 §5: internal/roster's tick loop owns its hosts while up to eight
# attempts run on goroutines of their own and Close cancels and joins them —
# joined in PR 2's first code commit, the one that creates it), and the model
# catalog cache (plan 030 §3.14: writers of one provider contend for its
# flock across goroutines in its tests, the older finishing last — joined in
# C16, the commit that creates it), and the spawn machinery (plan 032 §3.16:
# each started host's reaper goroutine races its terminate and every ready
# read's own goroutine — joined in C8, the commit that moves it out of
# internal/cli), and the hub (plan 032 §3.5: its accept loop, a goroutine per
# connection, its watch loop, its sweep and its teardown race each other under
# the lifecycle lock — joined in C10, the commit that creates it), and the
# ChatGPT sign-in (plan 033 §3.10: one token source per process serves every
# session's steps at once through a singleflight, while subscribers, the
# usage latch and Values are read from other goroutines, and its tests race a
# refresh against a sign-out, a sign-in and a second process — joined in C13,
# the commit that creates it) with the lock and durable write it builds on
# (internal/atomicfile).
# Packages run concurrently, so the wall clock is about the slowest
# of them. CI runs this same target, so a local pass and a CI pass mean the
# same thing; the two flakes that reached main in 2026-09 only ever showed
# under -race. The timeout is 20m because internal/tui runs every frame golden
# three times from plan 027 C29 on — once per command-gate mode in process, and
# once over the control socket — which took its -race run here from about 190s
# to 250s (it was about 60s before C17's gate modes): macOS CI measured
# internal/tui's -race run at 636s and 547s (ubuntu's at about 431s), so the
# old 15m would be clipped by the next slower runner.
test-race:
	CRAZE_GOLDEN_TRANSPORT=both go test -timeout 20m -race ./internal/acp ./internal/agent ./internal/engine/... ./internal/host ./internal/tui ./internal/harness/... ./internal/journal/... ./internal/transcript/... ./internal/protocol/... ./internal/control/... ./internal/remote/... ./internal/fakehost/... ./internal/rundir/... ./internal/backend/... ./internal/cli ./internal/roster ./internal/modelcache ./internal/hostspawn ./internal/hub ./internal/chatgptauth ./internal/atomicfile

test-cli:
	@if [ ! -f tests/cli/pyproject.toml ]; then echo "tests/cli not present yet"; exit 0; fi
	cd tests/cli && uv sync --frozen && uv run pytest -v

docs:
	uv sync --locked --group docs && uv run --locked zensical build --strict

docs-serve:
	uv sync --locked --group docs && uv run --locked zensical serve

clean:
	rm -rf bin
