# GitHub webhook setup

Janus triggers pipelines from GitHub **push** and **pull request** webhooks.

## 1. Run Janus with a secret

GitHub signs each webhook with an **HMAC-SHA256** of the raw body, sent as
`X-Hub-Signature-256: sha256=<hex>`. Janus recomputes the MAC with your secret
and compares it in constant time, rejecting mismatches with `401`. The endpoint
is only enabled when a secret is set.

```sh
janus serve \
  --addr :8080 \
  --data-dir /var/lib/janus \
  --github-secret "$(openssl rand -hex 24)"
```

The secret may also come from the `JANUS_GITHUB_SECRET` environment variable.
Without a secret, `/webhooks/github` returns `404` (disabled).

Janus must be reachable from GitHub (a public URL or a tunnel). Terminate TLS in
front of it in production.

## 2. Add the webhook in GitHub

In your repository: **Settings → Webhooks → Add webhook**.

- **Payload URL:** `https://janus.example.com/webhooks/github`
- **Content type:** `application/json`
- **Secret:** the same value passed to `--github-secret`
- **Events:** choose *"Let me select individual events"* and check **Pushes**
  and **Pull requests** (GitHub delivers tag pushes on **Pushes** too, so
  [`on.push.tags`](pipeline-reference.md#tag-filters) needs no extra box)
- **SSL verification:** enable it (recommended)

Click **Add webhook**; GitHub sends a `ping` (which Janus ignores with `200`) to
verify connectivity.

An optional `?pipeline_path=` query parameter on the URL selects a committed
pipeline file other than the configured default, named relative to the pipeline
directory (`?pipeline_path=release.yml` → `.janus/release.yml`; subdirectories
allowed, escapes rejected — the same rules as the manual API's field). GitHub
allows several webhooks per repository, so different hooks can route to
different pipelines.

## 3. What Janus does with each event

| GitHub event | Action |
|--------------|--------|
| `push` to `refs/heads/…` | Checks out `after` (the new commit) on the pushed branch; `before` is kept as the diff base for [`paths` filters](pipeline-reference.md#path-filters). |
| `push` to `refs/tags/…` | Checks out `head_commit.id` — the *commit* the tag names — and records the tag. Only workflows declaring [`on.push.tags`](pipeline-reference.md#tag-filters) run; the rest ignore it. `before` is not kept: a tag push has no diff base. |
| `pull_request` (`opened`/`reopened`/`synchronize`) | Checks out the PR's head commit; matched against the **base** (target) branch. |
| Branch/tag deletion | Ignored (`deleted`, or the `after` SHA is all zeros). |
| Other refs (`refs/pull/…`) / PR actions / event types | Ignored. |

The event is matched against the workflow's `on:` filters exactly as for GitLab:
**push** matches `on.push` against the pushed branch (with `paths`/`paths-ignore`
still able to skip it), and a pull request matches `on.merge_request` against the
**base branch** — so `on.merge_request.branches: [main]` runs for PRs landing on
`main`, and `${{ branch }}` is the base branch. (Janus normalizes a GitHub pull
request to its provider-neutral `merge_request` event, so one pipeline works
across GitLab and GitHub.)

Matching happens *after* the delivery is answered: every accepted event gets a
`202` with `{"status":"accepted","run_id":...}` immediately, and the checkout,
parse, and `on:` match run in the background. A non-matching event's run is
recorded as **skipped**; a checkout or pipeline error is recorded as **failed**.

> **Fork pull requests** are out of scope for v1 (the same limit as GitLab fork
> MRs): the checkout assumes the head branch is fetchable from the repo Janus
> clones.

## 4. Response codes

| Code | Meaning |
|------|---------|
| 202  | Delivery accepted; a run was recorded (`run_id` in the body). |
| 200  | Ignored event type (`ping`, branch/tag deletion, a non-branch non-tag ref, non-actionable PR action). |
| 403  | `{"status":"rejected"}` — the repository is not in the allowlist. |
| 503  | Janus is at capacity or its store is unavailable (`Retry-After` set) — GitHub retries. |
| 401  | Signature mismatch (`X-Hub-Signature-256`). |
| 404  | `github` provider not configured (no `--github-secret`). |

## Cloning over SSH

Janus checks out the payload's HTTPS `clone_url` by default. Set
[`clone_url: "ssh"`](configuration.md#ssh-clone-urls) to use `ssh_url`
(`git@github.com:owner/repo.git`) instead — note that `allow_repos` entries must
then be written in that scp-style form, and the service user needs its own key
and `known_hosts`. `clone_url` is a single server-wide setting shared by every
provider.

## Restricting which repos can run

Janus runs the triggered repo's pipeline as **host processes with no isolation**.
Configure `allow_repos` so a leaked webhook secret can't run an arbitrary
repository:

```yaml
# janus.yml
allow_repos:
  - https://github.com/acme        # only repos under the acme org
```

`allow_repos` is **deny-by-default**: with none set, every delivery is rejected
with **403**. Use `"*"` to allow all. See
[configuration.md](configuration.md#repository-allowlist) for matching rules.

## Reporting status back to GitHub

Set a token with the **`repo:status`** scope and Janus reports each run's state
to GitHub's Commit Statuses API, so pass/fail shows natively on the commit and
pull request:

```yaml
# janus.yml
github_api_token: "ghp_…"          # or JANUS_GITHUB_API_TOKEN
base_url: "https://ci.example.com" # optional; adds a link to the run page
# github_url: "https://github.example.com"  # GitHub Enterprise Server only
```

The token is **distinct from `github_secret`** (the inbound webhook secret),
sent as an `Authorization: Bearer` header and never logged. The repository the
status is posted to is **derived from the clone URL that `allow_repos` gated**, not
from the payload's `repository.full_name`, so a forged delivery cannot pair an
allowlisted clone URL with another repository and have this token write there. The
commit is likewise required to be a full object id. One status row per commit
(context `janus`) updates through the run's lifecycle:

| Run state | GitHub status |
|-----------|---------------|
| running   | `pending` (GitHub has no "running") |
| success   | `success` |
| failed    | `failure` |
| cancelled | `error` (the run did not complete — distinct from a job failure) |
| skipped / pending | *not posted* (a skipped run never validated the commit) |

The API base is `api.github.com` for github.com; for **GitHub Enterprise Server**
set `github_url` to the instance's web base (e.g. `https://github.example.com`)
and Janus posts to `…/api/v3`. With `clone_url: "ssh"` an scp-style URL has no
derivable API base, so `github_url` is required (set it to `https://github.com`
for github.com over SSH). Reporting is best-effort — a slow or failing GitHub
never fails or blocks a run.
