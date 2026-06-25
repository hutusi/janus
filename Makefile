BINARY  := janus
PKG     := ./...
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build test race cover fmt fmt-check vet lint ci clean

## build: compile the single static binary
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/janus

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

## ci: the full local gate — what CI enforces
ci: fmt-check vet lint race

## clean: remove build artifacts
clean:
	rm -f $(BINARY) coverage.out
