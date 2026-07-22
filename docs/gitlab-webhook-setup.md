# GitLab webhook setup

Janus triggers pipelines from GitLab **push** and **merge request** webhooks.

## 1. Run Janus with a secret

GitLab authenticates webhooks with a plaintext **secret token** sent in the
`X-Gitlab-Token` header. Janus compares it in constant time and rejects
mismatches with `401`. The endpoint is only enabled when a secret is set.

```sh
janus serve \
  --addr :8080 \
  --data-dir /var/lib/janus \
  --gitlab-secret "$(openssl rand -hex 24)"
```

The secret may also come from the `JANUS_GITLAB_SECRET` environment variable.
Without a secret, `/webhooks/gitlab` returns `404` (disabled).

Janus must be reachable from GitLab (a public URL or a tunnel). Terminate TLS in
front of it in production.

## 2. Add the webhook in GitLab

In your project: **Settings → Webhooks**.

- **URL:** `https://janus.example.com/webhooks/gitlab`
- **Secret token:** the same value passed to `--gitlab-secret`
- **Trigger:** check **Push events** and **Merge request events**
- **SSL verification:** enable it (recommended)

Click **Add webhook**, then **Test → Push events** to verify connectivity.

An optional `?pipeline_path=` query parameter on the URL selects a committed
pipeline file other than the configured default, named relative to the pipeline
directory (`?pipeline_path=release.yml` → `.janus/release.yml`; subdirectories
allowed, escapes rejected — the same rules as the manual API's field). GitLab
allows several webhooks per project, so different hooks can route to different
pipelines:

- `https://janus.example.com/webhooks/gitlab` — runs `.janus/ci.yml`
- `https://janus.example.com/webhooks/gitlab?pipeline_path=release.yml` — runs
  `.janus/release.yml`

An invalid value answers `200 {"status":"error"}` naming the pipeline path
(2xx so a URL typo cannot make GitLab auto-disable the hook); the selected file
is recorded on the run's event.

## 3. What Janus does with each event

| GitLab event          | Action |
|-----------------------|--------|
| Push Hook             | Checks out `after` (the new commit) on the pushed branch. |
| Merge Request Hook    | On `open`/`reopen`/`update`, checks out the MR's head commit. |
| Branch deletion       | Ignored (the `after` SHA is all zeros). |
| MR `merge`/`close`/…  | Ignored. |
| Other event types     | Ignored. |

The event is matched against the workflow's `on:` filters:

- **push** matches when `on.push` is declared and the **pushed branch** passes
  its filter — in `on.push.branches` (an empty/omitted list matches all
  branches), or not in `on.push.branches-ignore`.
- **merge_request** matches against the **target branch** — so
  `on.merge_request.branches: [main]` runs for MRs landing on `main`, and
  `branches-ignore: [main]` runs for MRs landing anywhere else. For merge
  requests, `${{ branch }}` is the target branch.

Matching happens *after* the delivery is answered: every accepted event gets a
`202` with `{"status":"accepted","run_id":...}` immediately, and the checkout,
parse, and `on:` match run in the background. A non-matching event's run is
recorded as **skipped** (with the reason on the run); a checkout or pipeline
error is recorded as **failed**. The delivery response never waits on git, so
GitLab's ~10-second webhook timeout cannot fail a delivery whose clone is
merely slow.

## 4. Response codes

| Code | Meaning |
|------|---------|
| 202  | Delivery accepted; a run was recorded (`run_id` in the body). Its outcome — success, failed + reason (bad checkout/pipeline), or skipped + reason (branch didn't match) — appears on the run in the dashboard and `GET /api/runs/{id}`. |
| 200  | Ignored event type (branch deletion, MR merge/close, non-push/MR hooks). |
| 403  | `{"status":"rejected"}` — the repository is not in the allowlist (see below). |
| 503  | Janus is at capacity or its store is unavailable (`Retry-After` set) — GitLab retries rather than dropping the event. |
| 401  | Secret token mismatch. |
| 404  | `gitlab` provider not configured (no `--gitlab-secret`). |

Every accepted delivery logs `webhook trigger accepted` with the clone URL
before any git work — if a run sits pending unusually long, that line in the
service journal shows exactly which URL its background checkout is fetching.

## Multiple repositories

One Janus server serves any number of projects — there is **no per-repo
registration**. Every delivery carries its repository's clone URL, and Janus
clones *that* repo for *that* run, reading the pipeline committed **in it**
(`pipeline_path`, default `.janus/ci.yml`; a hook's `?pipeline_path=` query
parameter picks a different committed file) — so each project defines its own
pipeline simply by committing one. To add a project:

1. Point its webhook at the **same** URL and secret as every other project.
2. Make sure its URL passes `allow_repos` (a group prefix covers a whole team).
3. Commit its `.janus/ci.yml`.

The single shared secret is by design: a leaked secret can only fake events for
allowlisted repositories, which run only their own committed pipelines at
commits that actually exist — worst case is a build of legitimate code, not
code injection. Rotate it by editing `janus.env` and restarting.

## Restricting which repos can run

Janus runs the triggered repo's pipeline as **host processes with no isolation**.
Configure `allow_repos` so a leaked webhook secret can't be used to run an
arbitrary repository:

```yaml
# janus.yml
allow_repos:
  - https://gitlab.example.com/acme   # only repos under the acme group
```

`allow_repos` is **deny-by-default**: with none set, every delivery is rejected
with **403**. Use `"*"` to allow all. A rejected delivery returns 403, so a
misconfigured allowlist surfaces immediately — confirm with GitLab's
**Test → Push events** button after changing it. (Repeated 4xx can make GitLab
auto-disable a webhook, so fix the allowlist rather than leaving it failing.)
See [configuration.md](configuration.md#repository-allowlist) for matching rules.

## Notes & limits

- **Authentication to clone:** private repos rely on the host's git
  configuration (SSH agent, credential helper, `.netrc`). Janus does not manage
  credentials. Checkouts run git non-interactively (`GIT_TERMINAL_PROMPT=0`,
  ssh in `BatchMode`), so missing credentials fail the run in seconds with the
  git/ssh error recorded as its reason — never hanging on a prompt no daemon
  can answer.
- **Cloning over SSH:** Janus checks out the payload's `git_http_url` by
  default. Set [`clone_url: "ssh"`](configuration.md#ssh-clone-urls) to use
  `git_ssh_url` instead — note that `allow_repos` entries must then be written
  in the SSH form the platform sends, and the service user still needs its own
  key and `known_hosts`. A host missing from `known_hosts` or a
  passphrase-protected key fails fast (`Host key verification failed.` /
  `Permission denied`) rather than blocking; an operator-set `GIT_SSH_COMMAND`
  or `GIT_SSH` is respected verbatim and must itself be non-interactive. If the
  platform advertises an SSH URL that is unreachable from the Janus host (a
  Docker-internal hostname or wrong port in `git_ssh_url`), the run fails with
  `Connection timed out` / `Could not resolve hostname` as its reason — fix
  the platform's external SSH domain/port setting rather than Janus.
- **Same-repo merge requests:** the fallback ref fetch assumes the MR source
  branch exists in the project. Fork MRs are out of scope for v1.
- GitHub/Gitea are not implemented — the `provider.Provider` interface is the
  seam to add them.
