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
| `pipeline_path` | `--pipeline-path` | `.janus/ci.yml` | Path to the pipeline file **inside each triggered repository** — not one server-wide pipeline; different repos naturally run their own committed pipelines. A manual trigger may override it per run via the request's `pipeline_path` field, naming a file **relative to the configured file's directory** (`"release.yml"` → `.janus/release.yml`; subdirectories allowed, escapes rejected) — so only files deliberately placed with the pipelines are runnable, and callers need not know where pipelines live. Webhooks may override it per hook with a `?pipeline_path=` query parameter on the webhook URL (same relative-name rules). |
| `max_parallel_jobs` | `--max-parallel-jobs` | `4` | Max jobs running concurrently **within** one run. `0` means the default; negatives are a startup error. |
| `max_parallel_runs` | `--max-parallel-runs` | `4` | Max runs executing concurrently. Excess runs queue as `pending`, bounded at 4× this cap (checkout and parse count too); beyond that, triggers get `503` with `Retry-After`. `0` means the default; negatives are a startup error. |
| `history_limit` | `--history-limit` | `1000` | Max terminal runs to retain. When exceeded, the oldest terminal runs — and their logs — are deleted after each run finishes and at startup; running/pending runs are never pruned. **The default is nonzero, so the first startup after upgrading prunes terminal runs beyond 1000 (and their logs).** Set `0` for unlimited retention; negatives are a startup error. This bounds the retained **run count** and the flat-file store's per-list scan (which reads every run directory) — it is **not** a total-disk cap — see `log_limit` for the per-step log bound, and use OS quotas for a hard ceiling. A run directory whose record cannot be read at all (an interrupted write, a corrupt file) is listed as `(unreadable run record)` with status `cancelled` so that retention can reclaim it too, rather than leaking silently. |
| `log_limit` | `--log-limit` | `10485760` (10 MiB) | Max bytes a single step may write to its log. Past it the log keeps its **head**, gains a `janus: log truncated at the configured log_limit` marker, and the rest is dropped — the step itself runs to completion and its exit code is unaffected. The head is kept because that is where the command and its first error are, and because the stored log is append-only: `?follow=1` advances a monotonic offset, so dropping bytes from the front would corrupt a live reader. **The default is nonzero**, so a runaway step is bounded out of the box; set `0` for unlimited. Negatives are a startup error. Applies to `janus run` too, where logs are held in memory. |
| `step_timeout` | `--step-timeout` | `0s` | Fail any step running longer than this (e.g. `"10m"`). `0` disables; negatives are a startup error. |
| `keep_workspaces` | `--keep-workspaces` | `false` | Don't delete workspaces after runs (debugging). |
| `workspace_strategy` | `--workspace-strategy` | `"fresh"` | `"fresh"`: a new directory per run, removed afterward. `"persistent"`: one reusable directory per repo — see [Persistent workspaces](#persistent-workspaces). `"mirror"`: a per-repo bare-mirror cache plus a fresh directory per run — see [Mirror workspaces](#mirror-workspaces). Any other value is a startup error. |
| `clone_url` | `--clone-url` | `"http"` | Which clone URL from the webhook payload to check out: `"http"` (the payload's `git_http_url`) or `"ssh"` (`git_ssh_url`). The chosen URL *is* the run's repo URL — it is what the allowlist gates, what the workspace clones, and what the run record shows — so switching to `"ssh"` means `allow_repos` entries must be written in SSH form too. See [SSH clone URLs](#ssh-clone-urls). Any other value is a startup error. |
| `gitlab_secret` | `--gitlab-secret` (`$JANUS_GITLAB_SECRET`) | _(empty)_ | GitLab webhook token. Enables `POST /webhooks/gitlab`. |
| `gitlab_api_token` | `--gitlab-api-token` (`$JANUS_GITLAB_API_TOKEN`) | _(empty)_ | Outbound GitLab API token (`api` scope) enabling [commit-status reporting](gitlab-commit-status.md). Distinct from `gitlab_secret`. |
| `gitlab_url` | _(none)_ | _(empty)_ | GitLab instance base URL for commit status; derived from the clone URL for `clone_url: http`, required for `ssh`/self-hosted subpaths. File-only. |
| `github_secret` | `--github-secret` (`$JANUS_GITHUB_SECRET`) | _(empty)_ | GitHub webhook secret (HMAC-SHA256). Enables `POST /webhooks/github`. |
| `github_api_token` | `--github-api-token` (`$JANUS_GITHUB_API_TOKEN`) | _(empty)_ | Outbound GitHub token (`repo:status` scope) enabling [commit-status reporting](github-webhook-setup.md#reporting-status-back-to-github). Distinct from `github_secret`. |
| `github_url` | _(none)_ | _(empty)_ | GitHub Enterprise Server web base for commit status (the `/api/v3` prefix is added); leave empty for github.com, set for GHES or github.com over `clone_url: ssh`. File-only. |
| `gitee_secret` | `--gitee-secret` (`$JANUS_GITEE_SECRET`) | _(empty)_ | Gitee webhook secret (password or signature key). Enables `POST /webhooks/gitee`. Gitee has no commit-status API — run feedback is via [notifications](#notifications). |
| `gitcode_secret` | `--gitcode-secret` (`$JANUS_GITCODE_SECRET`) | _(empty)_ | GitCode webhook secret (token or HMAC signature key). Enables `POST /webhooks/gitcode`. GitCode has no commit-status API — run feedback is via [notifications](#notifications). |
| `api_token` | `--api-token` (`$JANUS_API_TOKEN`) | _(empty)_ | Bearer token for the API (see auth rules below). |
| `allow_repos` | `--allow-repos` (comma-separated) | _(empty)_ | Repositories permitted to run. See "Repository allowlist" below. |
| `base_url` | _(none)_ | _(empty)_ | Public base URL of this daemon (e.g. `https://ci.example.com`). When set, notifications include a link to the run page (`<base_url>/runs/<id>`). File-only. |
| `notifications` | _(none)_ | _(empty)_ | Outbound webhook endpoints POSTed a JSON run summary when a run finishes. Empty disables them. See [Notifications](#notifications) below. File-only (a list). |

In YAML, `step_timeout` is a string (`"10m"`, `"30s"`); on the flag it is a Go
duration (`--step-timeout 10m`). `allow_repos` is a YAML list; the
`--allow-repos` flag is a comma-separated string that **replaces** (not merges
with) the file list. The config file is a single YAML document — a second
`---` document is an error, not silently ignored.

Notes:

- The webhook secrets and API tokens have `$JANUS_*` environment fallbacks so
  they can stay out of the file: `$JANUS_API_TOKEN`, `$JANUS_GITLAB_SECRET`,
  `$JANUS_GITLAB_API_TOKEN`, `$JANUS_GITHUB_SECRET`, `$JANUS_GITHUB_API_TOKEN`,
  `$JANUS_GITEE_SECRET`, and `$JANUS_GITCODE_SECRET`. The instance-URL settings
  (`gitlab_url`, `github_url`) are file-only, as are non-secret settings. Keep a
  config file holding secrets `chmod 600`.
- Without a GitLab secret, `/webhooks/gitlab` returns `404` (disabled).
- **`POST /api/trigger` always requires an API token** — it runs code on the
  host, so without a token it is **disabled** (`403`). Read endpoints
  (`GET /api/runs…`) require the token only when one is configured.
- The HTML dashboard (`/`, `/runs/{id}`) is **not** behind the API token. Put a
  reverse proxy in front if it must be protected.
- On `SIGINT`/`SIGTERM`, Janus stops accepting requests and waits up to 30s for
  in-flight runs to finish; overrunning runs are cancelled (process-group
  kill) and recorded as `cancelled`. After a crash or hard kill, the next
  startup marks the orphaned `pending`/`running` runs `cancelled`.

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
  - A trailing `.git` and each scheme's default port (`https` 443, `http` 80,
    `ssh` 22, `git` 9418) are normalized, and scheme + host are matched
    case-insensitively (paths are case-sensitive). Userinfo is **significant**:
    `ssh://git@host/…` and `ssh://root@host/…` are different authorities
    (git connects as that user) and do not match each other.
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

### Notifications

`notifications` is a list of outbound webhook endpoints. When a run reaches a
terminal state, Janus POSTs each subscribed endpoint a JSON summary of the run.
Delivery is **best-effort**: a slow, failing, or unreachable endpoint is logged
and never fails or blocks a run. See [Notifications](notifications.md) for the
full payload schema and behavior.

```yaml
base_url: "https://ci.example.com"   # optional; adds a run link to each payload
notifications:
  - url: "https://chat.example.com/hooks/janus"
    on: [failed]                     # default when omitted: failures only
    secret: "shared-token"           # optional Bearer token; keep this file chmod 600
  - url: "https://example.com/ci-events"
    on: [success, failed, cancelled, skipped]
```

- **`url`** (required) — an `http`/`https` endpoint. Invalid URLs are a startup error.
- **`on`** — which terminal outcomes deliver to this target: any of `success`,
  `failed`, `cancelled`, `skipped`. Omitted means **failures only**. An unknown
  value is a startup error.
- **`secret`** — optional; sent as an `Authorization: Bearer <secret>` header so
  the receiver can authenticate the delivery. There is no `JANUS_*` env fallback
  for a per-target secret (a list can't map to one variable), so keep the config
  file `chmod 600` when it holds one.
- **`base_url`** — when set, each payload carries `url = <base_url>/runs/<id>`
  linking to the run page; omit it to leave the link out.

Notifications are **daemon-only**: local `janus run` executes directly and never
notifies.

### GitLab commit status

Set `gitlab_api_token` (a GitLab access token, `api` scope) to report a run's
state back to GitLab's commit-status API, so pass/fail shows on the commit/MR.
`running` and the terminal state are posted (per-commit, context `janus`);
`skipped`/`pending` are not. The API base is derived from the clone URL for
`clone_url: http`; ssh mode and self-hosted subpaths need `gitlab_url`. Reuses
`base_url` for a link back to the run page. Best-effort — never fails or blocks a
run, and the token is sent as a `PRIVATE-TOKEN` header, never logged. See
[GitLab commit status](gitlab-commit-status.md).

### SSH clone URLs

By default Janus checks out the payload's `git_http_url`. If the host can only
clone over SSH — deploy keys, or HTTPS blocked outright — set:

```yaml
clone_url: "ssh"
allow_repos:
  - "git@gitlab.example.com:acme"
```

**`allow_repos` must move to the SSH form as well**, and in the *exact* form
your platform emits. The two forms do not cross-match: normalization only
canonicalizes strings containing `://`, so scp-style `git@host:group/repo` is
compared opaquely — its host is case-sensitive, and it will never match an
`ssh://git@host/group/repo` entry. Copy `git_ssh_url` verbatim out of a webhook
delivery and write your entry in that shape. GitLab sends scp-style on port 22
and `ssh://…` when the SSH port is non-default.

Leaving `allow_repos` on `https://` while setting `clone_url: "ssh"` yields
`403 rejected` — the allowlist gates the same string that gets cloned, by
design.

Janus still manages no credentials. The **service user** needs a passphrase-less
key and a pre-seeded `known_hosts` (a background service cannot answer SSH's
trust prompt) — with the packaged unit that is `/var/lib/janus/.ssh/`, since
`$HOME` is `/var/lib/janus`. See [deployment](deployment.md). Janus enforces
this: checkout git runs with `GIT_TERMINAL_PROMPT=0` and, when the operator
has configured **nothing** for the transport (no `GIT_SSH_COMMAND`/`GIT_SSH`
in the service environment, no `core.sshCommand` in the service user's
gitconfig), with `GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10
-o ServerAliveInterval=15 -o ServerAliveCountMax=4` — so an unprovisioned
`known_hosts` or a key that needs a passphrase fails the checkout in seconds
instead of hanging on a prompt until the checkout deadline. Likewise, unless
`GIT_ASKPASS` or `core.askpass` is configured, `GIT_ASKPASS` is set *empty*,
which disables askpass resolution entirely so credential requests hit the
blocked terminal path and fail immediately (`GIT_TERMINAL_PROMPT` alone does
not stop askpass helpers, and a blocking helper would pin the checkout slot).
The gitconfig checks run inside the workspace being checked out, so local
config and `includeIf "gitdir:"` rules count as configured too. The
connect bound and keepalive probes cover the network-shaped hangs too: a
`git_ssh_url` advertising a host or port unreachable from the Janus server (a
Docker-internal hostname is the classic case) fails in ~10 s with
`Connection timed out`, and a connection that stalls mid-transfer is aborted
in ~60 s — in every case the run fails with a named reason instead of sitting
pending until the deadline.
Host keys are deliberately still verified; nothing is auto-accepted. Anything
you configure yourself — `GIT_SSH_COMMAND`, `GIT_SSH`, `GIT_ASKPASS`,
`core.sshCommand`, `core.askpass` — is respected verbatim and must itself be
non-interactive (include `-o BatchMode=yes` in a custom ssh command). Verify
as the service user, with the SSH URL — that is what Janus will now clone:

```sh
sudo -u janus env HOME=/var/lib/janus git ls-remote git@gitlab.example.com:acme/app.git
```

If the platform omits `git_ssh_url` from its payload, the delivery fails with
`400` naming the missing field rather than quietly cloning over HTTPS.

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
  the next run recreates it. (A directory that is no longer a usable git
  repository — a missing or corrupt `.git` — rebuilds itself automatically, at
  the cost of one cold build. Nothing else does: a run cancelled mid-update, a
  fetch that cannot reach the remote, or a commit that is not there leaves the
  directory intact and falls back to a fresh per-run clone for that run, so a
  shutdown or a network blip never costs the next run its caches.)
- **Same-repo runs are serialized.** A trigger that arrives while another run
  of the same repo is building falls back to a fresh per-run directory for
  that one run — correct, just without the caches. Different repos are
  unaffected and run in parallel as usual.
- **Dirs survive restarts** and the startup sweep, and grow over time (fetched
  history plus whatever your builds leave behind).
- `keep_workspaces` still governs only the fresh/fallback `run-*` directories.
- A finished run's recorded `workspace_dir` points at a directory that later
  runs of the same repo will mutate.

### Mirror workspaces

`workspace_strategy: "mirror"` keeps a **bare mirror** of each repository at
`<workspace_root>/mirror-<hash-of-repo-URL>` and gives every run a fresh
`run-*` directory materialized from it — a local hardlink clone plus a
detached checkout, no network involved. Only the mirror talks to the remote,
and only when the triggering commit isn't already cached, so the network cost
of a busy repo converges to one incremental fetch per new commit. Choosing
between the caching strategies: `persistent` keeps untracked build caches warm
(`node_modules`, incremental-compiler state) at the price of hermeticity;
`mirror` keeps runs hermetic — every build starts from a pristine checkout —
and caches only the git objects.

How it behaves:

- **The mirror is an accelerator, never a gate.** If another run of the same
  repo is syncing the mirror (a per-repo try-lock, held for the fetch and any
  compaction it triggers), or the sync or local clone fails for any reason,
  the run proceeds with a plain direct-from-remote checkout — same-repo
  triggers never block, and a broken mirror never fails a run the direct path
  could serve.
- **Same-repo runs overlap freely.** Unlike `persistent`, materializing a
  workspace takes no lock: concurrent runs of one repo all clone from the
  same mirror.
- **Dirs survive restarts** and the startup sweep, and are compacted
  automatically: syncs that fetch end with a `git gc --auto` pass using git's
  own heuristics, so fetch-created packs consolidate and history rewritten
  away by force-pushes is pruned after git's standard grace period — a
  mirror's size tracks its repository's rather than growing forever. That
  grace period is fixed by Janus rather than read from your git config, so
  compaction can never discard a commit a run is about to check out; other gc
  tuning (how often it triggers, how aggressively it packs) still comes from
  git config as usual. What is *not* automatic is removal: a repository that
  stops building keeps its mirror until you delete the `mirror-*` directory
  (which is also the way to force a full reset — the next run rebuilds it). Structural corruption (a
  half-created or damaged mirror) rebuilds automatically, while fetch
  failures deliberately don't — a transient network error must not throw away
  a large healthy cache.
- `keep_workspaces` governs the materialized `run-*` checkouts as usual; it
  never keeps or removes the mirror.

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
