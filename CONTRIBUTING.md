# Contributing to Janus

## Prerequisites

- Go (see the version in [`go.mod`](go.mod))
- `git` on `PATH` (used by Janus to check out repositories)
- Optional: [`golangci-lint`](https://golangci-lint.run) v2 for local linting
  (`brew install golangci-lint`). CI runs it regardless.

## Layout

```
cmd/janus/        # CLI entrypoint (serve | run | validate)
internal/         # all implementation packages (not importable externally)
  pipeline/       # YAML parse + strict validation + interpolation  (pure, no I/O)
  model/          # shared spec + runtime types
  engine/         # DAG, scheduler, executor
  workspace/      # per-run git checkout
  provider/       # webhook providers (GitLab) + normalized Event
  store/          # run/log persistence (memory + file)
  server/         # HTTP API + read-only dashboard
docs/             # deep-dive documentation
testdata/         # YAML fixtures + captured webhook payloads
```

## Make targets

| Target            | Purpose                                            |
|-------------------|----------------------------------------------------|
| `make build`      | Compile the single static binary                   |
| `make test`       | Run all tests                                       |
| `make race`       | Run all tests under the race detector              |
| `make cover`      | Tests with coverage + total                        |
| `make fmt`        | Format code in place                               |
| `make fmt-check`  | Fail if anything is unformatted                    |
| `make vet`        | `go vet`                                            |
| `make lint`       | `golangci-lint` (if installed)                     |
| `make ci`         | The full gate: fmt-check + vet + lint + race tests |

Run **`make ci`** before every commit; it must be green.

On Windows the make targets require Git Bash with GNU Make; alternatively run
the underlying commands directly (`gofmt -l .`, `go vet ./...`,
`go test -race ./...`).

## Testing philosophy

- The pure core (`internal/pipeline`, `internal/engine/dag.go`) is developed
  **test-first** (red → green → refactor). It carries the spec's defining
  behavior — strict rejection of unsupported YAML — so its tests come first.
- The goroutine-based scheduler and streaming log writer must pass under
  `-race`; `make race`/`make ci` always run with it.
- Standard library only: `testing` + `net/http/httptest`. No assertion
  frameworks (keeps the dependency surface at exactly one module).

## Commit & PR conventions

- Work on a feature branch; do not commit to `main`.
- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
  `test:`, `docs:`, `refactor:`, `chore:`, …
- Each commit should be green (`make ci` passes) and update the docs it affects
  in the same commit.
