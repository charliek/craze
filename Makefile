.PHONY: build lint test test-cli docs docs-serve clean

# mise shims first so `make` works in a non-activated shell.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

VERSION ?= dev
LDFLAGS := -X github.com/charliek/craze/internal/version.Version=$(VERSION)

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/craze ./cmd/craze
	go build -o bin/craze-fake-agent ./cmd/craze-fake-agent

lint:
	golangci-lint run
	@n=$$(go list -f '{{join .Imports "\n"}}' ./internal/tui ./internal/cli | grep -c internal/acp || true); \
	echo "$$n"; test "$$n" = "0"

test:
	go test -timeout 5m -v ./...

test-cli:
	@if [ ! -f tests/cli/pyproject.toml ]; then echo "tests/cli not present yet"; exit 0; fi
	cd tests/cli && uv sync --frozen && uv run pytest -v

docs:
	uv sync --locked --group docs && uv run --locked zensical build --strict

docs-serve:
	uv sync --locked --group docs && uv run --locked zensical serve

clean:
	rm -rf bin
