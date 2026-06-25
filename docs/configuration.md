# Configuration

Janus is a single binary, `janus`, with three subcommands. All configuration is
via flags (and a couple of environment fallbacks); there is no config file.

## `janus serve`

Runs the HTTP server: webhooks, manual trigger, JSON API, and dashboard.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `:8080` | HTTP listen address. |
| `--data-dir` | _(empty)_ | Directory for persistent run history. **Empty = in-memory** (lost on restart). |
| `--workspace-root` | `$TMPDIR/janus-workspaces` | Where per-run checkouts are created (and swept on startup). |
| `--pipeline-path` | `.janus/ci.yml` | In-repo path to the pipeline file. |
| `--max-parallel-jobs` | `4` | Max jobs running concurrently **within** one run. |
| `--max-parallel-runs` | `4` | Max runs executing concurrently (excess runs queue as `pending`). |
| `--step-timeout` | `0` | Fail any step running longer than this (e.g. `10m`). `0` disables. |
| `--keep-workspaces` | `false` | Don't delete workspaces after runs (debugging). |
| `--gitlab-secret` | `$JANUS_GITLAB_SECRET` | GitLab webhook token. Enables `POST /webhooks/gitlab`. |
| `--api-token` | `$JANUS_API_TOKEN` | Bearer token for the API (see auth rules below). |

Notes:

- Without `--gitlab-secret`, `/webhooks/gitlab` returns `404` (disabled).
- **`POST /api/trigger` always requires `--api-token`** — it runs code on the
  host, so without a token it is **disabled** (`403`). Read endpoints
  (`GET /api/runs…`) require the token only when one is configured.
- The HTML dashboard (`/`, `/runs/{id}`) is **not** behind `--api-token`. Put a
  reverse proxy in front if it must be protected.
- On `SIGINT`/`SIGTERM`, Janus stops accepting requests and waits up to 30s for
  in-flight runs to finish.

## `janus run [flags] <dir>` / `janus run --repo ...`

Runs a pipeline locally, streaming logs to the terminal.

| Flag | Default | Purpose |
|------|---------|---------|
| `--file` | `.janus/ci.yml` | Pipeline file, relative to the workspace. |
| `--branch` | _(empty)_ | Value for `${{ branch }}`. |
| `--max-parallel-jobs` | `4` | Max jobs running concurrently. |
| `--step-timeout` | `0` | Per-step timeout. |
| `--repo` | _(empty)_ | Git URL to check out (instead of `<dir>`). |
| `--sha` | _(empty)_ | Commit to check out (with `--repo`). |
| `--ref` | _(empty)_ | Ref to fetch as a fallback (with `--repo`). |
| `--workspace-root` | `$TMPDIR` | Where to create the checkout (with `--repo`). |
| `--keep-workspace` | `false` | Don't delete the checkout afterward. |

Exit code is non-zero if the run does not succeed.

## `janus validate <file>`

Parses and validates a pipeline file, printing the workflow name and jobs, or a
descriptive error. Exit code is non-zero on invalid input.

## Step environment

Each step runs via `/bin/sh -c` with a **curated** environment, not the Janus
daemon's full environment:

- Passed through from the host (if set): `PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`,
  `TMPDIR`.
- Injected: `CI=true`, `JANUS_RUN_ID`, `JANUS_EVENT`, `JANUS_REF`, `JANUS_SHA`,
  `JANUS_BRANCH`.
- Overlaid (later wins): workflow `env` → job `env` → step `env`.
