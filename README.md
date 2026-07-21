# Janus

**Janus** is a minimal, self-hosted CI/CD service in a single dependency-free
binary. It runs CI pipelines **directly as host processes** (no containers, no
VMs), triggered by **git-server webhooks** (GitLab push & merge request) or a
**manual trigger**. Pipelines are a small, GitHub-Actions-flavored YAML stored
in the repository at `.janus/ci.yml`.

> **Guiding rule: minimal beats complete.** The pipeline YAML describes *what
> runs and in what order* — it is not a programming language. Anything beyond
> the supported keys (expressions, `if:`, matrix, `uses:`, templating) is a
> **validation error**, not a feature.

## Install

### Prebuilt binary (recommended)

Download the static binary for your platform from the
[latest release](https://github.com/hutusi/janus/releases/latest):

```sh
# Linux x86_64 — swap in janus-linux-arm64 / janus-darwin-{amd64,arm64} / janus-windows-{amd64,arm64}.exe
curl -fsSL -o janus \
  https://github.com/hutusi/janus/releases/latest/download/janus-linux-amd64
chmod +x janus
./janus version   # janus v0.1.0
```

Verify the download against `checksums.txt` from the same release (integrity, not
provenance — see [Security model](#security-model-read-this-before-deploying)):

```sh
sha256sum -c checksums.txt --ignore-missing
```

Janus runs on Linux, macOS, and **Windows**. On Windows, pipeline steps run under
`cmd` by default (or `powershell` / `pwsh` / `sh` via a step's
[`shell:`](docs/pipeline-reference.md#step-shell)); you run `janus serve` /
`janus run` from a console — the systemd unit and `deploy/install.sh` below are
Linux-only.

To run Janus as a Linux systemd service, [`deploy/install.sh`](deploy/install.sh)
does the whole thing (binary, dedicated user, config, secrets, unit) in one
command — see the [deployment guide](docs/deployment.md#quick-install-scripted).

### Build from source

Requires Go (see [`go.mod`](go.mod)). The version is baked in from the git tag:

```sh
make build   # produces ./janus
```

Install it (unix):

```sh
make build && sudo make install          # binary only → /usr/local/bin/janus (PREFIX= to relocate)
make build && make install-service       # Linux: full systemd provision via deploy/install.sh --binary
```

`make install` places just the binary; the service install (dedicated user,
config, secrets, unit) is [`deploy/install.sh`](deploy/install.sh) — see the
[deployment guide](docs/deployment.md).

On Windows, build with plain `go build` — the Makefile needs a POSIX shell
(`make build` only works under Git Bash with GNU Make, and writes an
extensionless `janus`). `CGO_ENABLED=0` is already the Go default there:

```powershell
go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" -o janus.exe ./cmd/janus
```

## Quickstart

```sh
# Build the single binary
make build

# Start the server (webhooks, manual trigger, JSON API, dashboard)
./janus serve --addr :8080 --data-dir ./janus-data

# In another shell
curl -s localhost:8080/healthz
# {"status":"ok","version":"dev"}
# Open the dashboard at http://localhost:8080/
```

Settings can also come from a YAML config file. Run `janus init` to scaffold a
commented `janus.yml`, then `janus serve` (it auto-loads `./janus.yml`, or pass
`--config PATH`). Flags and env vars override the file. See
[the example](internal/config/example.yml) and
[docs/configuration.md](docs/configuration.md).

Run a pipeline locally (logs stream to the terminal, prefixed by job):

```sh
# Against a directory that already contains .janus/ci.yml
janus run --branch main ./my-repo

# Or check out a repository at a specific commit first (shallow fetch)
janus run --repo https://gitlab.com/acme/app.git --sha <commit> --ref refs/heads/main
```

### HTTP API & dashboard

`janus serve` exposes:

| Method & path                     | Purpose |
|-----------------------------------|---------|
| `POST /api/trigger`               | Manually start a run: `{"repo_url","branch","sha","ref","pipeline_path"}` → `202 {"run_id"}` (requires `--api-token`; repo must be in the allowlist; `pipeline_path` optionally picks a committed pipeline file by name relative to the pipeline directory, e.g. `"release.yml"` → `.janus/release.yml`). The 202 is written before the checkout; poll `GET /api/runs/{id}` for the outcome — a checkout/pipeline failure appears there as a `failed` run with a reason. Unknown JSON fields are a `400`; when the run queue is full, `503` with `Retry-After` — retry later. |
| `GET /api/runs`                   | List run **summaries**, newest first (`?limit=&offset=` for paging); no per-step `jobs` — see the detail endpoint |
| `GET /api/runs/{id}`              | Run detail (job/step statuses, exit codes) |
| `GET /api/runs/{id}/logs`         | Combined logs; `?job=&step=` for one step; `?follow=1` to stream |
| `GET /healthz`                    | Health + version |
| `GET /` and `/runs/{id}`          | Read-only HTML dashboard |

```sh
# /api/trigger requires --api-token (it runs code on the host)
curl -XPOST localhost:8080/api/trigger \
  -H "Authorization: Bearer $JANUS_API_TOKEN" \
  -d '{"repo_url":"https://gitlab.com/acme/app.git","ref":"refs/heads/main","branch":"main"}'

# Optional pipeline_path runs a different committed file from the pipeline
# directory for this trigger only ("release.yml" → .janus/release.yml)
curl -XPOST localhost:8080/api/trigger \
  -H "Authorization: Bearer $JANUS_API_TOKEN" \
  -d '{"repo_url":"https://gitlab.com/acme/app.git","ref":"refs/heads/main","branch":"main","pipeline_path":"release.yml"}'
```

### GitLab webhooks

Run with a secret and persistent storage, then point a GitLab webhook at it:

```sh
janus serve --data-dir /var/lib/janus --gitlab-secret "$(openssl rand -hex 24)"
```

Push and merge-request events trigger runs that are matched against each
workflow's `on:` filters. Any number of repositories can share one server —
each runs its own committed pipeline. With `--data-dir`, run history and logs
survive restarts. See [docs/gitlab-webhook-setup.md](docs/gitlab-webhook-setup.md).

## Pipeline format (target)

```yaml
name: ci
on:
  push: { branches: [main] }
  merge_request: { branches: [main] }   # GitLab term; normalized internally
env: { CI: "true" }                      # also per-job and per-step
jobs:
  build:
    steps:
      - run: npm ci
        working-directory: ./app         # optional
  test:
    needs: [build]                        # DAG
    steps:
      - run: npm test
```

Variables are limited to `${{ env.VAR }}` plus `ref` / `sha` / `short_sha` /
`branch` / `event`. See [docs/pipeline-reference.md](docs/pipeline-reference.md)
for the full grammar and the list of rejected constructs.

## Design

Single Go binary, standard library throughout; the only third-party module is
`gopkg.in/yaml.v3`. Documentation:

- [docs/architecture.md](docs/architecture.md) — package map, domain model, run lifecycle
- [docs/pipeline-reference.md](docs/pipeline-reference.md) — full YAML grammar + what's rejected
- [docs/configuration.md](docs/configuration.md) — config file, all settings, the allowlist
- [docs/gitlab-webhook-setup.md](docs/gitlab-webhook-setup.md) — wiring a GitLab webhook
- [docs/deployment.md](docs/deployment.md) — run as a hardened systemd service on Linux
- [examples/](examples/) — ready-to-copy sample pipelines (build, release)
- [CHANGELOG.md](CHANGELOG.md) — notable changes

### Security model (read this before deploying)

Jobs run as **host processes with no isolation** from the machine Janus runs on.
A pipeline can do anything the `janus` user can. Run Janus as a dedicated,
unprivileged user on a host you control — see [docs/deployment.md](docs/deployment.md)
for a hardened systemd unit. Container/VM isolation is intentionally out of scope.

Because a triggered repo's pipeline executes on the host, restrict which repos
can run with the **`allow_repos` allowlist** — it's defense-in-depth against a
leaked webhook secret or API token being used to point Janus at an
attacker-controlled repository. It is **deny-by-default**: with nothing
configured every webhook and manual trigger is rejected (403); set `allow_repos`
to the hosts/groups you trust, or `"*"` to allow all. See
[docs/configuration.md](docs/configuration.md#repository-allowlist).

**Binary integrity vs. provenance.** The release `checksums.txt` verifies a
downloaded binary's **integrity** — that it wasn't corrupted or truncated — but
not its **provenance**: it ships from the same GitHub release as the binary and
is not separately signed, so the trust root is simply HTTPS and that release.
Building from source (`make build`) doesn't change that root, but lets you audit
what you run instead of trusting an opaque binary; the installer can then deploy
your own build with `deploy/install.sh --binary ./janus` (no download step).

## Out of scope

containers/VMs · matrix · `uses:` / third-party actions · `if:` / expressions ·
distributed runners · artifact storage · caching · secrets beyond host env · a
Windows service installer (the binary runs on Windows, but only the systemd unit
and `deploy/install.sh` are provided). Node/Go/Python are assumed to be installed
on the host.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). TL;DR: `make ci` runs the full gate
(format check, vet, lint, race tests).

## License

Janus is released under the MIT License — see [LICENSE](LICENSE).
