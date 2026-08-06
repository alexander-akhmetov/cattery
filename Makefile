.PHONY: all build test test-go test-python test-ts lint lint-go lint-ts fmt install clean

# install target destination. `cattery setup` writes the absolute path of the
# binary into kitty.conf and Claude's hooks, so this only has to be somewhere on
# PATH.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

all: build

build:
	go build -o cattery ./cmd/cattery

test: test-go test-python test-ts

# -race matches CI, so a data race in the picker's git fan-out fails here too.
test-go:
	go test -race -timeout 5m ./...

# Covers the watcher, the tab module, and the default tab bar. None of them
# needs kitty: the tests stub it.
test-python:
	python3 -m unittest discover -s tests -p '*_test.py'

test-ts: node_modules
	npm test

lint: lint-go lint-ts

lint-go:
	golangci-lint run
	go mod tidy -diff
	go tool govulncheck ./...
	go tool deadcode -test ./...

lint-ts: node_modules
	npm run typecheck

# The pi extension is the only part with a JS toolchain, and it is a dev
# dependency of the tests alone: nothing that gets installed needs node.
node_modules: package-lock.json
	npm ci
	@touch node_modules

fmt:
	golangci-lint fmt

# Installs the binary only. Run `cattery setup` after this to write the kitty
# files and the configuration they need.
install: build
	install -d $(BINDIR)
	install -m 0755 cattery $(BINDIR)/cattery

clean:
	rm -f cattery
	go clean ./...
