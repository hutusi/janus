# GitCode webhook setup

Janus triggers pipelines from GitCode (gitcode.com) **push** and **merge
request** webhooks. GitCode's webhooks use GitLab's payload format, so setup
mirrors [GitLab](gitlab-webhook-setup.md) closely — only the header names differ.

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
- **Events:** check **Push** and **Merge Request**

An optional `?pipeline_path=` query parameter selects a committed pipeline file
other than the configured default, with the same relative-name rules as the
other providers.

## 3. What Janus does with each event

| GitCode event (`X-GitCode-Event`) | Action |
|-----------------------------------|--------|
| `Push Hook` | Checks out `after` on the pushed branch; `before` is the diff base for [`paths` filters](pipeline-reference.md#path-filters). |
| `Merge Request Hook` (`open`/`reopen`/`update`) | Checks out the MR's head commit; matched against the **target** branch. |
| `Tag Push Hook` / branch deletion | Ignored. |
| Other actions / event types | Ignored. |

Because the payload is GitLab-shaped, the mapping is identical to GitLab's — a
merge request matches on its **target** branch, `${{ branch }}` is the target,
and `clone_url: "http" | "ssh"` selects `git_http_url` vs `git_ssh_url`. The same
`.janus/ci.yml` works across every provider Janus supports.

## 4. Response codes

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
