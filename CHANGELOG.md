# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Linux deployment** — a hardened `systemd` unit ([`deploy/janus.service`](deploy/janus.service)) and secret template ([`deploy/janus.env.example`](deploy/janus.env.example)), plus a [deployment guide](docs/deployment.md) for running Janus as a dedicated-user service.

## [0.1.0] - 2026-06-26

### Added

- **Pipeline engine & CLI** — a small GitHub-Actions-flavored `.janus/ci.yml` with strict validation (unsupported keys and expressions are errors, not features), a DAG scheduler that runs independent jobs concurrently and is fail-fast, and a host-process step executor that streams combined stdout/stderr to the store. `janus validate <file>` and `janus run <dir>`.
- **Server & triggers** — `janus serve` exposes a GitLab webhook endpoint (push + merge request), a manual HTTP trigger, a JSON API, and a read-only HTML dashboard. Each run shallow-checks-out the triggering commit into a per-run workspace.
- **Persistence** — a restart-safe flat-file run/log store, plus an in-memory store for the CLI and tests.
- **Hardening** — per-step process-group kill, step timeouts, run/job concurrency caps, graceful shutdown, and bearer auth on `/api` (mandatory for `POST /api/trigger`).
- **YAML config file** — `--config` (and auto-load of `./janus.yml`) covering all `janus serve` settings, with precedence defaults < file < env < flags. `janus init` scaffolds an annotated `janus.yml`.
- **Repository allowlist** (`allow_repos`) — deny-by-default with a `*` allow-all escape hatch; a repo not on the list is rejected with `403` before any checkout. Defense-in-depth against a leaked webhook secret or API token.
- **`CLAUDE.md`** — concise guidance for AI coding agents.
- **MIT license.**

### Changed

- The example config now defaults `data_dir`/`workspace_root` to a cwd-relative `./janus-data` (created on demand, no sudo) instead of `/var/lib/janus`.

[unreleased]: https://github.com/hutusi/janus/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/hutusi/janus/releases/tag/v0.1.0
