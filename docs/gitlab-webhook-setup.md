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

## 3. What Janus does with each event

| GitLab event          | Action |
|-----------------------|--------|
| Push Hook             | Checks out `after` (the new commit) on the pushed branch. |
| Merge Request Hook    | On `open`/`reopen`/`update`, checks out the MR's head commit. |
| Branch deletion       | Ignored (the `after` SHA is all zeros). |
| MR `merge`/`close`/…  | Ignored. |
| Other event types     | Ignored. |

The event is matched against the workflow's `on:` filters:

- **push** matches when `on.push` is declared and the **pushed branch** is in
  `on.push.branches` (an empty/omitted list matches all branches).
- **merge_request** matches against the **target branch** — so
  `on.merge_request.branches: [main]` runs for MRs landing on `main`. For merge
  requests, `${{ branch }}` is the target branch.

A non-matching event returns `200` with `{"status":"ignored"}` (so GitLab does
not disable the hook). A started run returns `202` with `{"run_id"}`.

## 4. Response codes

| Code | Meaning |
|------|---------|
| 202  | A run was started (`run_id` in the body). |
| 200  | Event accepted but no run (ignored type, or branch didn't match). |
| 200  | `{"status":"error"}` — the repo's `.janus/ci.yml` is missing/invalid, or checkout failed (logged server-side). |
| 401  | Secret token mismatch. |
| 404  | `gitlab` provider not configured (no `--gitlab-secret`). |

## Notes & limits

- **Authentication to clone:** private repos rely on the host's git
  configuration (SSH agent, credential helper, `.netrc`). Janus does not manage
  credentials.
- **Same-repo merge requests:** the fallback ref fetch assumes the MR source
  branch exists in the project. Fork MRs are out of scope for v1.
- GitHub/Gitea are not implemented — the `provider.Provider` interface is the
  seam to add them.
