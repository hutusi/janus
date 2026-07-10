# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Per-trigger pipeline file** — `POST /api/trigger` accepts an optional `pipeline_path` field selecting a committed pipeline file other than the configured default (e.g. `.janus/release.yml`) for that one run. The override must live in the configured pipeline file's directory (`.janus/` by default) — paths outside it, absolute paths, and `..` escapes are rejected with `400` before anything is cloned — and it is recorded on the run's event, so `GET /api/runs/{id}` shows which file ran. Webhooks keep using the server-wide `pipeline_path`.
- **Windows support** — pipeline steps can run on native Windows. The step shell now depends on the host (`/bin/sh` on unix, `cmd` on Windows) and is overridable with a new step [`shell:`](docs/pipeline-reference.md#step-shell) key (`sh`/`bash`/`cmd`/`powershell`/`pwsh`). A `janus-windows-<arch>.exe` binary is published, and a `windows-latest` CI job guards execution (process-tree kill uses the built-in `taskkill`, so no new dependency). Pipelines are no longer portable across OSes; the systemd unit and `deploy/install.sh` remain Linux-only.
- **Scripted install** — [`deploy/install.sh`](deploy/install.sh) provisions Janus as a systemd service in one command: it detects the architecture, downloads and checksum-verifies the release binary, creates the `janus` user, writes an idempotent config plus a `janus.env` with generated secrets, and enables the unit. Supports `--allow-repo`, `--version`, `--dry-run`, and an `upgrade` mode; linted by `shellcheck` in CI.
- **Offline install** — `deploy/install.sh --binary <path>` installs a locally-built binary and makes no network access at all (no release download or remote checksum), for air-gapped hosts. `upgrade` accepts it too.
- **Linux deployment** — a hardened `systemd` unit ([`deploy/janus.service`](deploy/janus.service)) and secret template ([`deploy/janus.env.example`](deploy/janus.env.example)), plus a [deployment guide](docs/deployment.md) for running Janus as a dedicated-user service.
- **Example pipelines** — [`examples/build.yml`](examples/build.yml) (build on every master update) and [`examples/release.yml`](examples/release.yml) (build, then publish the output to a separate pages repo), with an [`examples/README.md`](examples/README.md) covering the push-vs-MR-merge trigger, the shallow checkout, and host SSH auth. Validated in CI.

### Fixed

- **systemd hardening now actually applies** — the balanced sandbox in [`deploy/janus.service`](deploy/janus.service) had trailing inline comments on its directive lines, which systemd parses as part of the value and silently ignores. `NoNewPrivileges`, `ProtectSystem`, `ProtectHome` and `PrivateTmp` were all being dropped (confirmed via `systemctl show`); the comments moved to their own lines so every protection takes effect. CI now rejects inline-comment directives in `deploy/*.service`.

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
