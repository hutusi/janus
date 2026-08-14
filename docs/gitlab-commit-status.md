# GitLab commit status

> For **GitHub**, the equivalent is `github_api_token` — see
> [github-webhook-setup.md › Reporting status back to GitHub](github-webhook-setup.md#reporting-status-back-to-github).

Janus can report a run's state back to GitLab's [Commit Status API](https://docs.gitlab.com/ee/api/commits.html#set-the-pipeline-status-of-a-commit),
so a build's progress and result show natively on the commit and merge request
(the ✓/✗ next to the commit, and the MR's pipeline widget) — without leaving
GitLab. It complements the generic [notifications](notifications.md) webhook:
notifications push a summary *out* to chat/automation; commit status closes the
loop *back* to GitLab.

It reports **GitLab webhook-triggered runs only** — the status is keyed on the
project id and commit GitLab sends in the webhook. A manual API trigger (provider
`manual`, no project id) and local `janus run` never report.

## Enabling

Set an outbound token — a [personal or project access token](https://docs.gitlab.com/ee/user/profile/personal_access_tokens.html)
with the **`api`** scope, from an account that can post statuses on the project:

```yaml
gitlab_secret: "…"                       # inbound webhook token (already required to accept GitLab webhooks)
gitlab_api_token: "glpat-…"              # outbound API token; enables commit status
base_url: "https://ci.example.com"       # optional; adds a link from the status to the run page
# gitlab_url: "https://gitlab.example.com"  # see "Instance URL" below
```

Prefer the **`JANUS_GITLAB_API_TOKEN`** env var (or `--gitlab-api-token`) over the
file. The API token is **distinct from `gitlab_secret`**: `gitlab_secret` is the
inbound `X-Gitlab-Token` Janus checks on incoming webhooks; `gitlab_api_token` is
the outbound credential Janus sends (as a `PRIVATE-TOKEN` header) to GitLab. It is
never logged.

The project a status is posted to is **derived from the clone URL that
`allow_repos` gated**, not taken from the payload's `project.id`: Janus sends the
URL-encoded `NAMESPACE/PROJECT` path (which GitLab accepts wherever `:id` appears,
e.g. `/api/v4/projects/acme%2Fapp/statuses/…`), so a forged delivery cannot pair an
allowlisted clone URL with another project and have this token write there. Nested
groups and an instance hosted on a subpath are handled. Scoping the token to just
the projects it must write to — a *project* access token rather than a personal
one — is still good practice.

## Instance URL

The API base (`<base>/api/v4/…`) is resolved as:

- **`clone_url: "http"` (default):** derived from the webhook's clone URL
  (`https://gitlab.example.com/acme/app.git` → `https://gitlab.example.com`). No
  extra config needed.
- **`clone_url: "ssh"`, a self-hosted instance on a subpath, or to override:**
  set **`gitlab_url`** to the instance base (e.g. `https://gitlab.example.com`).
  An scp-style ssh clone URL (`git@host:acme/app.git`) has no derivable HTTPS
  authority, so `gitlab_url` is **required** in ssh mode — without it, statuses
  are skipped (with a startup warning).

A derived base means the *webhook payload* names the host the token-bearing
request goes to, and `allow_repos` is the gate that keeps that host one you
chose. With a wildcard (`"*"`) entry and no `gitlab_url`, any host that can
deliver an accepted webhook can receive the token — Janus warns about this
combination at startup. Scope `allow_repos`, or pin the instance with
`gitlab_url`.

## What is reported

One status row per commit (context **`janus`**), updated across the run:

| Run state | GitLab state | Posted? |
|-----------|--------------|---------|
| running (execution starts) | `running` | yes |
| success | `success` | yes |
| failed | `failed` | yes |
| cancelled | `canceled` | yes |
| skipped (no `on:` match, path/branch filter, all jobs skipped) | — | **no** |
| pending / queued | — | no |

`running` is posted just before execution begins, then one terminal state when
the run settles. **`skipped` is deliberately not reported** — a skipped run means
the workflow didn't apply to this commit, so a green check (which would say CI
*passed*) or a red one would both be wrong; the honest signal is no status. The
status carries a `target_url` to the run page when `base_url` is set, and a
`description` naming the workflow and outcome.

> If you gate merges on a **required** "janus" status check, note that a
> path/branch-skipped push posts nothing, so that check stays unfilled and the MR
> can't merge. (Reporting `success` for skipped runs is a possible future opt-in;
> today the default is not to post.)

## Semantics

- **Best-effort — never fails or blocks a run.** Posts for one commit are
  delivered **in order** (FIFO) through a fixed pool of bounded worker queues, so
  `running` always precedes the terminal and Janus never posts to one commit
  concurrently. Each post has a timeout; a slow, failing (`>= 300`), or unreachable
  GitLab is logged and dropped, and if a queue is full further posts to it are
  dropped. Posts are drained on shutdown.
- **Single attempt, one conflict retry.** A `409` (GitLab's documented
  concurrent-update response) is retried once; any other failure is dropped, not
  retried. Because `running` is posted before the build and the terminal after it,
  a crash between them (or a dropped terminal post) can leave the commit showing
  `running`; re-running or re-pushing refreshes it.
- **Only after durable persistence.** The terminal status is posted only once the
  run's terminal state is recorded in the store, so GitLab never shows a result
  the daemon didn't keep.
- **Security.** The token travels as a `PRIVATE-TOKEN` header (never in a URL or a
  log); redirects are not followed (so the token can't be forwarded across an
  `https`→`http` downgrade); a clone URL's userinfo is dropped when deriving the
  API base.
