.PHONY: build lint test test-race test-cli docs docs-serve clean

# mise shims first so `make` works in a non-activated shell.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

VERSION ?= dev
LDFLAGS := -X github.com/charliek/craze/internal/version.Version=$(VERSION)

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/craze ./cmd/craze
	go build -o bin/craze-fake-agent ./cmd/craze-fake-agent

# The go list check below is one half of craze's import boundary: internal/tui
# and internal/cli must not import internal/acp. The other half is the
# depguard rule in .golangci.yml, which golangci-lint runs: internal/harness
# must not import the rest of craze (D-02; plan 018 §3.1).
lint:
	golangci-lint run
	@n=$$(go list -f '{{join .Imports "\n"}}' ./internal/tui ./internal/cli | grep -c internal/acp || true); \
	echo "$$n"; test "$$n" = "0"

test:
	go test -timeout 5m -v ./...

# The packages with concurrency worth the 10x slowdown: the ACP client's
# reader and writer goroutines, the session's event fan-out, the TUI's
# clipboard seams and leaked command goroutines, the host Hub's per-reporter
# workers, and the native harness (its turn runner persists from stream
# callbacks while cancel and Close run on other goroutines; the whole tree is
# named, so each harness package is raced as it lands). Packages run
# concurrently, so the wall clock is about the slowest of them. CI runs this
# same target, so a local pass and a CI pass mean the same thing; the two
# flakes that reached main in 2026-09 only ever showed under -race.
test-race:
	go test -timeout 5m -race ./internal/acp ./internal/agent ./internal/host ./internal/tui ./internal/harness/...

test-cli:
	@if [ ! -f tests/cli/pyproject.toml ]; then echo "tests/cli not present yet"; exit 0; fi
	cd tests/cli && uv sync --frozen && uv run pytest -v

docs:
	uv sync --locked --group docs && uv run --locked zensical build --strict

docs-serve:
	uv sync --locked --group docs && uv run --locked zensical serve

clean:
	rm -rf bin
