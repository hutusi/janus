# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Janus — a minimal self-hosted CI/CD service in a single dependency-free Go binary. It runs pipelines as **host processes (no containers/VMs)**, triggered by GitLab webhooks (push + merge request), a manual HTTP API, or the CLI. A pipeline is a small GitHub-Actions-flavored YAML at `.janus/ci.yml` in the *triggered* repo.

**Minimal beats complete**: the pipeline YAML says *what runs and in what order* — it is not a programming language. Anything beyond the supported keys (`if:`, expressions, `matrix:`, `uses:`, templating) must be a **validation error, not a feature** (enforced in `internal/pipeline`). The only third-party module is `gopkg.in/yaml.v3`; keep it that way, and write tests with the stdlib only (no testify).

## Commands

```bash
make ci      # pre-commit gate: gofmt check, go vet, golangci-lint (v2), go test -race
make build   # CGO_ENABLED=0 static binary ./janus
make race    # go test -race ./...
make cover   # tests + coverage total
go test ./internal/pipeline/ -run TestParseRejects   # one test (subtest: -run 'TestAllows/deny_userinfo_confusion')
```

CLI subcommands: `janus init` (scaffold `janus.yml`), `janus serve` (auto-loads `./janus.yml`), `janus run <dir>`, `janus validate <file>`.

## Development Workflow

- Branch per feature off `main` (`<type>/<topic>`); never commit to `main`. Push and open PRs only when asked.
- Conventional Commits; each commit green under `make ci`; the body explains the why. No `Co-Authored-By` or AI-attribution trailers on commits or PR descriptions.
- Ship docs with the change: update the relevant `docs/` page and add a `CHANGELOG.md` (Unreleased) entry in the same commit as the behavior.

## Architecture & docs

Single binary (`cmd/janus`); all logic under `internal/`, depending inward on `internal/model`. Run lifecycle: `server` → `runner.Trigger` (validate, record pending run, answer 202) → async: `workspace.Checkout` → `pipeline.Parse` → match `on:` → `engine.Execute` (DAG scheduler → per-step `os/exec`); pre-execution outcomes land on the run as `failed`/`skipped` + reason.

- [docs/architecture.md](docs/architecture.md) — package map, two-layer domain model, run lifecycle, concurrency/safety **invariants & gotchas** (strict `KnownFields`, `runState.update` as the single mutation path, process-group kill)
- [docs/pipeline-reference.md](docs/pipeline-reference.md) — supported YAML grammar + what's rejected and why
- [docs/configuration.md](docs/configuration.md) — config file, precedence (defaults < file < env < flags), the repo allowlist
- [docs/gitlab-webhook-setup.md](docs/gitlab-webhook-setup.md) — wiring a GitLab webhook
- [docs/deployment.md](docs/deployment.md) — Linux systemd deployment (`deploy/janus.service`, dedicated user, balanced sandbox)
- `README.md` — overview, quickstart, security model
