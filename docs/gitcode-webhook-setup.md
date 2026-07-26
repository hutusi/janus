# GitCode webhook setup

Janus triggers pipelines from GitCode (gitcode.com) **push**, **tag push**, and
**merge request** webhooks. GitCode's webhooks use GitLab's payload format, so
setup mirrors [GitLab](gitlab-webhook-setup.md) closely — only the header names
differ.

## 1. Run Janus with a secret

GitCode authenticates each webhook one of two ways, which Janus both accept
against the same configured secret (constant-time):

- a plaintext token in `X-GitCode-Token` (like GitLab's `X-Gitlab-Token`), or
- an HMAC-SHA256 body signature in `X-GitCode-Signature-256: sha256=<hex>`
  (like GitHub's `X-Hub-Signature-256`).

The endpoint is only enabled when a secret is set.

```sh
janus serve \
  --addr :8080 \
  --data-dir /var/lib/janus \
  --gitcode-secret "$(openssl rand -hex 24)"
```

The secret may also come from `JANUS_GITCODE_SECRET`. Without a secret,
`/webhooks/gitcode` returns `404` (disabled).

## 2. Add the webhook in GitCode

In your repository: **管理 / Settings → WebHooks → Add**.

- **URL:** `https://janus.example.com/webhooks/gitcode`
- **密码 / 签名密钥 (password / signing key):** the same value passed to
  `--gitcode-secret`
- **Events:** check **Push** and **Merge Request** — plus **Tag Push** if any
  pipeline declares [`on.push.tags`](pipeline-reference.md#tag-filters)

An optional `?pipeline_path=` query parameter selects a committed pipeline file
other than the configured default, with the same relative-name rules as the
other providers.

## 3. What Janus does with each event

| GitCode event (`X-GitCode-Event`) | Action |
|-----------------------------------|--------|
| `Push Hook` | Checks out `after` on the pushed branch; `before` is the diff base for [`paths` filters](pipeline-reference.md#path-filters). |
| `Tag Push Hook` | Checks out `checkout_sha` — the *commit* the tag names — and records the tag. Only workflows declaring [`on.push.tags`](pipeline-reference.md#tag-filters) run; the rest ignore it. `before` is not kept: a tag push has no diff base. |
| `Merge Request Hook` (`open`/`reopen`/`update`) | Checks out the MR's head commit; matched against the **target** branch. |
| Branch/tag deletion | Ignored. |
| Other actions / event types | Ignored. |

Because the payload is GitLab-shaped, the mapping is identical to GitLab's — a
merge request matches on its **target** branch, `${{ branch }}` is the target,
and `clone_url: "http" | "ssh"` selects `git_http_url` vs `git_ssh_url`. The same
`.janus/ci.yml` works across every provider Janus supports.

> **Fork / cross-repo merge requests are out of scope for v1** (the same limit as
> GitLab): the merge request is cloned from its target project, so a request whose
> source branch lives in a *fork* is not fetchable and the run fails checkout.
> Same-repository merge requests — the common case — work.

## 4. Restricting which repos can run

Janus runs the triggered repo's pipeline as **host processes with no isolation**.
Configure `allow_repos` so a leaked webhook secret can't be used to run an
arbitrary repository:

```yaml
# janus.yml
allow_repos:
  - https://gitcode.com/acme        # only repos under the acme org
```

`allow_repos` is **deny-by-default**: with none set, every delivery is rejected
with **403** — so a Janus that is otherwise configured correctly will still
turn away every push until you set this. Use `"*"` to allow all. See
[configuration.md](configuration.md#repository-allowlist) for matching rules,
and note that `clone_url: ssh` requires the entries in SSH form.

## 5. Response codes

Identical to the other providers: `202` accepted, `200` ignored event, `403`
repo not in the [allowlist](configuration.md#repository-allowlist), `503`
at capacity/store unavailable, `401` token/signature mismatch, `404` provider
not configured.

## Reporting run results

GitCode exposes **no commit-status API** (it ships its own native "Actions" CI),
so Janus cannot post a native pass/fail check to a GitCode commit or merge
request. Use the generic [notifications](notifications.md) instead — a
`notifications:` target delivers a JSON run summary when a run finishes, for
every provider. The dashboard and `GET /api/runs/{id}` always show the outcome
as well.
