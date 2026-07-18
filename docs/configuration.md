# Configuration

Janus is a single binary, `janus`, with a few subcommands. The server
(`janus serve`) is configured from four sources, applied in increasing order of
precedence:

```text
built-in defaults  <  --config YAML file  <  environment variables  <  CLI flags
```

So a flag always wins, but an unset flag never overrides a value from the file
or environment with its default. A config file is optional — flags alone work.

## `janus serve`

Runs the HTTP server: webhooks, manual trigger, JSON API, and dashboard.

### Config file

`--config PATH` (or `$JANUS_CONFIG`) points at a YAML file. When `--config` is
not given, `janus serve` auto-loads **`./janus.yml`** if it exists; otherwise it
runs on built-in defaults. Unknown keys, an explicitly-named missing file, or a
malformed value are **startup errors** — the server refuses to start rather than
running with a misread config.

Run **`janus init`** to scaffold a commented `janus.yml` (it won't overwrite an
existing file without `--force`); see the
[annotated example](../internal/config/example.yml).

| YAML key | Flag | Default | Purpose |
|----------|------|---------|---------|
| `addr` | `--addr` | `:8080` | HTTP listen address. |
| `data_dir` | `--data-dir` | _(empty)_ | Directory for persistent run history. **Empty = in-memory** (lost on restart). |
| `workspace_root` | `--workspace-root` | `$TMPDIR/janus-workspaces` | Where per-run checkouts are created (and swept on startup). |
| `pipeline_path` | `--pipeline-path` | `.janus/ci.yml` | Path to the pipeline file **inside each triggered repository** — not one server-wide pipeline; different repos naturally run their own committed pipelines. A manual trigger may override it per run via the request's `pipeline_path` field, naming a file **relative to the configured file's directory** (`"release.yml"` → `.janus/release.yml`; subdirectories allowed, escapes rejected) — so only files deliberately placed with the pipelines are runnable, and callers need not know where pipelines live. Webhooks always use the configured path. |
| `max_parallel_jobs` | `--max-parallel-jobs` | `4` | Max jobs running concurrently **within** one run. `0` means the default; negatives are a startup error. |
| `max_parallel_runs` | `--max-parallel-runs` | `4` | Max runs executing concurrently (excess runs queue as `pending`). `0` means the default; negatives are a startup error. |
| `step_timeout` | `--step-timeout` | `0s` | Fail any step running longer than this (e.g. `"10m"`). `0` disables; negatives are a startup error. |
| `keep_workspaces` | `--keep-workspaces` | `false` | Don't delete workspaces after runs (debugging). |
| `workspace_strategy` | `--workspace-strategy` | `"fresh"` | `"fresh"`: a new directory per run, removed afterward. `"persistent"`: one reusable directory per repo — see [Persistent workspaces](#persistent-workspaces). Any other value is a startup error. |
| `gitlab_secret` | `--gitlab-secret` (`$JANUS_GITLAB_SECRET`) | _(empty)_ | GitLab webhook token. Enables `POST /webhooks/gitlab`. |
| `api_token` | `--api-token` (`$JANUS_API_TOKEN`) | _(empty)_ | Bearer token for the API (see auth rules below). |
| `allow_repos` | `--allow-repos` (comma-separated) | _(empty)_ | Repositories permitted to run. See "Repository allowlist" below. |

In YAML, `step_timeout` is a string (`"10m"`, `"30s"`); on the flag it is a Go
duration (`--step-timeout 10m`). `allow_repos` is a YAML list; the
`--allow-repos` flag is a comma-separated string that **replaces** (not merges
with) the file list. The config file is a single YAML document — a second
`---` document is an error, not silently ignored.

Notes:

- Only the two secrets have environment fallbacks (`$JANUS_GITLAB_SECRET`,
  `$JANUS_API_TOKEN`) so they can stay out of the file. Other settings are
  file-or-flag. Keep a config file holding secrets `chmod 600`.
- Without a GitLab secret, `/webhooks/gitlab` returns `404` (disabled).
- **`POST /api/trigger` always requires an API token** — it runs code on the
  host, so without a token it is **disabled** (`403`). Read endpoints
  (`GET /api/runs…`) require the token only when one is configured.
- The HTML dashboard (`/`, `/runs/{id}`) is **not** behind the API token. Put a
  reverse proxy in front if it must be protected.
- On `SIGINT`/`SIGTERM`, Janus stops accepting requests and waits up to 30s for
  in-flight runs to finish.

### Repository allowlist

`allow_repos` controls which repositories a webhook or manual trigger may run.
A triggered repo is cloned and its `.janus/ci.yml` runs as **host processes with
no isolation**, so this is the guard against a leaked webhook secret / API token
being used to run an attacker-controlled repo.

- **Deny by default.** An empty or omitted `allow_repos` rejects every webhook
  and manual trigger with **403** (the server still starts, logging a warning).
- **`*` allows all.** A single `"*"` entry permits any repository — a deliberate,
  greppable opt-out.
- **Entries are scheme-aware URL prefixes with a path boundary.** An entry
  matches a repo URL when they are equal or the URL continues after a `/`. So:
  - `https://gitlab.example.com` → any repo on that host.
  - `https://gitlab.example.com/acme` → any repo under the `acme` group, but
    **not** `…/acmecorp` and **not** the look-alike host
    `https://gitlab.example.com.evil.com/…`.
  - A trailing `.git` and the default port (`:443`/`:80`/`:22`) are normalized,
    and scheme + host are matched case-insensitively (paths are case-sensitive).
  - URLs whose path contains `.` or `..` segments — literal or percent-encoded
    (`%2e`, `%2f`, `%25`) — are **always denied**: git or the filesystem could
    resolve them across the prefix boundary after the match. An allowlist
    *entry* containing such segments is a **startup error**.
- **Each scheme/host is explicit.** `http://` does not match an `https://` entry.
  A bare host with no scheme (e.g. `gitlab.example.com`) is a **startup error**.
- **Scope.** The allowlist gates `POST /api/trigger` and `/webhooks/*` only.
  `janus run --repo` (local CLI) is **not** gated — it's operator-local.

```yaml
allow_repos:
  - https://gitlab.example.com/acme
  - https://gitlab.example.com/platform
```

### Persistent workspaces

`workspace_strategy: "persistent"` gives each repository **one reusable
workspace** at `<workspace_root>/persist-<hash-of-repo-URL>` instead of a fresh
directory per run. A trigger updates it in place — `git fetch` plus
`git reset --hard` to the requested commit — so tracked files always match the
commit, while **untracked files survive**: `node_modules`, build caches,
incremental-compiler state. That's the point — repeat builds skip cold
dependency installs.

What you trade and how it behaves:

- **Not hermetic.** A build can be affected by leftovers from previous runs.
  Delete the repo's `persist-*` directory any time to force a clean rebuild —
  the next run recreates it. (Any git failure in the reuse path — a corrupt
  directory, a stale lock file, an unfetchable commit — also triggers an
  automatic rebuild from scratch, at the cost of one cold build.)
- **Same-repo runs are serialized.** A trigger that arrives while another run
  of the same repo is building falls back to a fresh per-run directory for
  that one run — correct, just without the caches. Different repos are
  unaffected and run in parallel as usual.
- **Dirs survive restarts** and the startup sweep, and grow over time (fetched
  history plus whatever your builds leave behind).
- `keep_workspaces` still governs only the fresh/fallback `run-*` directories.
- A finished run's recorded `workspace_dir` points at a directory that later
  runs of the same repo will mutate.

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

## `janus init [flags]`

Writes a commented starter config file and exits.

| Flag | Default | Purpose |
|------|---------|---------|
| `--config` | `janus.yml` | Path to write. |
| `--force` | `false` | Overwrite an existing file (otherwise it errors). |

## `janus validate <file>`

Parses and validates a pipeline file, printing the workflow name and jobs, or a
descriptive error. Exit code is non-zero on invalid input.

## Step environment

Each step runs via the step's shell — the host default (`/bin/sh -c` on unix,
`cmd /C` on Windows) or the step's [`shell:`](pipeline-reference.md#step-shell) —
with a **curated** environment, not the Janus daemon's full environment:

- Passed through from the host (if set): `PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`,
  `TMPDIR`, plus the Windows equivalents (`SystemRoot`, `ComSpec`, `PATHEXT`,
  `USERPROFILE`, `TEMP`/`TMP`, …).
- Injected: `CI=true`, `JANUS_RUN_ID`, `JANUS_EVENT`, `JANUS_REF`, `JANUS_SHA`,
  `JANUS_BRANCH`.
- Overlaid (later wins): workflow `env` → job `env` → step `env`.
