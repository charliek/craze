.PHONY: build lint test test-cli clean

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

test:
	go test -timeout 5m -v ./...

test-cli:
	@if [ ! -f tests/cli/pyproject.toml ]; then echo "tests/cli not present yet"; exit 0; fi
	cd tests/cli && uv sync --frozen && uv run pytest -v

clean:
	rm -rf bin
