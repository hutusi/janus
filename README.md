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

## Status

Built incrementally. Current capabilities are tracked per phase:

- [x] **Phase 0** — project skeleton, quality gate (lint/test/CI), `janus serve` with `/healthz`
- [x] **Phase 1** — pipeline parsing, strict validation, variable interpolation (`janus validate`)
- [x] **Phase 2** — DAG scheduler + host-process executor (`janus run`)
- [x] **Phase 3** — per-run git workspace (shallow checkout of the triggering SHA)
- [x] **Phase 4** — HTTP server, manual trigger, read-only dashboard
- [x] **Phase 5** — persistent run history + GitLab webhook
- [x] **Phase 6** — hardening (process-group kill, timeouts, concurrency caps, auth)

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
| `POST /api/trigger`               | Manually start a run: `{"repo_url","branch","sha","ref"}` → `202 {"run_id"}` |
| `GET /api/runs`                   | List runs, newest first (`?limit=`) |
| `GET /api/runs/{id}`              | Run detail (job/step statuses, exit codes) |
| `GET /api/runs/{id}/logs`         | Combined logs; `?job=&step=` for one step; `?follow=1` to stream |
| `GET /healthz`                    | Health + version |
| `GET /` and `/runs/{id}`          | Read-only HTML dashboard |

```sh
curl -XPOST localhost:8080/api/trigger \
  -d '{"repo_url":"https://gitlab.com/acme/app.git","ref":"refs/heads/main","branch":"main"}'
```

### GitLab webhooks

Run with a secret and persistent storage, then point a GitLab webhook at it:

```sh
janus serve --data-dir /var/lib/janus --gitlab-secret "$(openssl rand -hex 24)"
```

Push and merge-request events trigger runs that are matched against each
workflow's `on:` filters. With `--data-dir`, run history and logs survive
restarts. See [docs/gitlab-webhook-setup.md](docs/gitlab-webhook-setup.md).

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
(added in Phase 1) for the full grammar and the list of rejected constructs.

## Design

Single Go binary, standard library throughout; the only third-party module is
`gopkg.in/yaml.v3`. Documentation:

- [docs/architecture.md](docs/architecture.md) — package map, domain model, run lifecycle
- [docs/pipeline-reference.md](docs/pipeline-reference.md) — full YAML grammar + what's rejected
- [docs/configuration.md](docs/configuration.md) — all flags and the step environment
- [docs/gitlab-webhook-setup.md](docs/gitlab-webhook-setup.md) — wiring a GitLab webhook

### Security model (read this before deploying)

Jobs run as **host processes with no isolation** from the machine Janus runs on.
A pipeline can do anything the `janus` user can. Run Janus as a dedicated,
unprivileged user on a host you control. Container/VM isolation is intentionally
out of scope.

## Out of scope

containers/VMs · matrix · `uses:` / third-party actions · `if:` / expressions ·
distributed runners · artifact storage · caching · secrets beyond host env ·
Windows. Node/Go/Python are assumed to be installed on the host.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). TL;DR: `make ci` runs the full gate
(format check, vet, lint, race tests).

## License

TBD.
