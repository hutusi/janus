# Gitee webhook setup

Janus triggers pipelines from Gitee (码云) **push** and **pull request**
webhooks.

## 1. Run Janus with a secret

Gitee authenticates each webhook with the `X-Gitee-Token` header in one of two
modes, chosen when you create the hook:

- **密码 / Password** — the plaintext secret is sent in the header.
- **签名 / Signature** — an HMAC-SHA256 over `<X-Gitee-Timestamp>\n<secret>`
  (keyed by the secret), base64- then URL-encoded.

Janus accepts **either** mode against the same configured secret and compares in
constant time, so you don't have to tell it which you picked. The endpoint is
only enabled when a secret is set.

```sh
janus serve \
  --addr :8080 \
  --data-dir /var/lib/janus \
  --gitee-secret "$(openssl rand -hex 24)"
```

The secret may also come from `JANUS_GITEE_SECRET`. Without a secret,
`/webhooks/gitee` returns `404` (disabled).

## 2. Add the webhook in Gitee

In your repository: **管理 / Manage → WebHooks → 添加 (Add)**.

- **URL:** `https://janus.example.com/webhooks/gitee`
- **WebHook 密码/签名密钥:** the same value passed to `--gitee-secret` (either
  mode works)
- **事件 (Events):** check **Push** and **Pull Request** — plus **Tag Push** if
  any pipeline declares [`on.push.tags`](pipeline-reference.md#tag-filters)

An optional `?pipeline_path=` query parameter selects a committed pipeline file
other than the configured default, with the same relative-name rules as the
other providers.

## 3. What Janus does with each event

| Gitee event | Action |
|-------------|--------|
| `Push Hook` | Checks out `after` on the pushed branch; `before` is the diff base for [`paths` filters](pipeline-reference.md#path-filters). |
| `Tag Push Hook` | Checks out `head_commit.id` — the *commit* the tag names — and records the tag. Only workflows declaring [`on.push.tags`](pipeline-reference.md#tag-filters) run; the rest ignore it. `before` is not kept: a tag push has no diff base. |
| `Merge Request Hook` (`open`/`update`) | Checks out the PR's head commit; matched against the **target** branch. |
| Branch/tag deletion | Ignored. |
| Other actions / event types | Ignored. |

A Gitee pull request normalizes to the same provider-neutral `merge_request`
event as GitLab and GitHub, matched against the **target** branch — so one
`.janus/ci.yml` works across all three hosts, and `${{ branch }}` is the target
branch. Gitee sends both GitHub-style (`clone_url`/`ssh_url`) and GitLab-style
(`git_http_url`/`git_ssh_url`) clone URLs; Janus uses whichever the selected
[`clone_url`](configuration.md#ssh-clone-urls) transport provides.

## 4. Restricting which repos can run

Janus runs the triggered repo's pipeline as **host processes with no isolation**.
Configure `allow_repos` so a leaked webhook secret can't be used to run an
arbitrary repository:

```yaml
# janus.yml
allow_repos:
  - https://gitee.com/acme          # only repos under the acme org
```

`allow_repos` is **deny-by-default**: with none set, every delivery is rejected
with **403** — so a Janus that is otherwise configured correctly will still
turn away every push until you set this. Use `"*"` to allow all. Confirm with
Gitee's **Test** button after changing it. See
[configuration.md](configuration.md#repository-allowlist) for matching rules,
and note that `clone_url: ssh` requires the entries in SSH form.

## 5. Response codes

Identical to the other providers: `202` accepted, `200` ignored event, `403`
repo not in the [allowlist](configuration.md#repository-allowlist), `503`
at capacity/store unavailable, `401` token/signature mismatch, `404` provider
not configured.

## Reporting run results

Gitee has **no commit-status API**, so Janus cannot post a native pass/fail
check to a Gitee commit or pull request. Use the generic
[notifications](notifications.md) instead — a `notifications:` target delivers a
JSON run summary to chat, on-call, or automation when a run finishes, and works
for every provider. (The dashboard and `GET /api/runs/{id}` always show the
outcome as well.)
