BINARY  := janus
PKG     := ./...
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin

.DEFAULT_GOAL := build

.PHONY: build install uninstall install-service test race cover fmt fmt-check vet lint lint-sh lint-unit ci clean

## build: compile the single static binary
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/janus

## install: install the built binary to $(BINDIR) (run `make build` first; sudo as needed)
install:
	@test -x $(BINARY) || { echo "no ./$(BINARY) binary — run 'make build' first"; exit 1; }
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)

## uninstall: remove the installed binary from $(BINDIR)
uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY)

## install-service: provision the systemd service from the local build (Linux; wraps deploy/install.sh)
install-service:
	@test -x $(BINARY) || { echo "no ./$(BINARY) binary — run 'make build' first"; exit 1; }
	sudo deploy/install.sh --binary ./$(BINARY) $(INSTALL_FLAGS)

## test: run all tests
test:
	go test $(PKG)

## race: run all tests under the race detector
race:
	go test -race $(PKG)

## cover: run tests with coverage and print the total
cover:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

## fmt: format all Go code in place
fmt:
	gofmt -w .

## fmt-check: fail if any Go file is not gofmt-formatted
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt-formatted:"; echo "$$unformatted"; exit 1; \
	fi

## vet: run go vet
vet:
	go vet $(PKG)

## lint: run golangci-lint if installed (CI always runs it via the official action)
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed locally; skipping (CI runs it)"; \
	fi

## lint-sh: run shellcheck on shell scripts if installed (CI always runs it)
lint-sh:
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck deploy/*.sh; \
	else \
		echo "shellcheck not installed locally; skipping (CI runs it)"; \
	fi

## lint-unit: reject systemd directives with trailing inline comments (silently ignored)
lint-unit:
	@if grep -nE '^[A-Za-z][A-Za-z0-9]*=.*[[:space:]]#' deploy/*.service; then \
		echo "systemd directives must not have trailing inline comments — move them to their own line"; \
		exit 1; \
	fi

## ci: the full local gate — what CI enforces
ci: fmt-check vet lint lint-sh lint-unit race

## clean: remove build artifacts
clean:
	rm -f $(BINARY) coverage.out
