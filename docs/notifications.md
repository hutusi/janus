# Notifications

Janus can POST a JSON summary of a run to one or more webhook endpoints when the
run finishes. This is the way to get build results out of Janus without polling
the dashboard or the `GET /api/runs` API — into chat, an on-call system, or your
own automation.

Notifications are configured in `janus.yml` (server config), not in a
repository's pipeline file: the pipeline grammar stays minimal, and one place
controls delivery for every repo the daemon serves. See the
[configuration reference](configuration.md#notifications) for the config keys.

## Configuration

```yaml
base_url: "https://ci.example.com"   # optional; adds a run link to each payload
notifications:
  - url: "https://chat.example.com/hooks/janus"
    on: [failed]                     # default when omitted: failures only
    secret: "shared-token"           # optional Bearer token
  - url: "https://example.com/ci-events"
    on: [success, failed, cancelled, skipped]
```

| Key | Required | Meaning |
|-----|----------|---------|
| `url` | yes | The `http`/`https` endpoint to POST. An invalid URL is a startup error. |
| `on` | no | Which terminal outcomes deliver to this target: any of `success`, `failed`, `cancelled`, `skipped`. Omitted = **failures only**. An unknown value is a startup error. |
| `secret` | no | Sent as an `Authorization: Bearer <secret>` header so the receiver can authenticate the delivery. |
| `base_url` | no | Top-level (not per-target). When set, each payload includes a run link joined structurally under it (`<base_url>/runs/<id>`). Must be a plain `http`/`https` URL — userinfo, a query, or a fragment is a startup error (it would leak into or break every link). |

Because `notifications` is a list, per-target secrets have no `JANUS_*` env
fallback (unlike `gitlab_secret` / `api_token`) — they live in the file. **Keep
`janus.yml` `chmod 600`** when it holds a secret.

## Payload

Each delivery is a single `POST` with `Content-Type: application/json` and this
body (a failing run shown):

```json
{
  "run_id": "a1b2c3d4",
  "workflow": "ci",
  "status": "failed",
  "reason": "failed jobs: build",
  "provider": "gitlab",
  "event": "push",
  "repo_url": "https://gitlab.example.com/acme/app.git",
  "branch": "main",
  "ref": "refs/heads/main",
  "commit": "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c",
  "commit_title": "Fix the flaky test",
  "created_at": "2026-07-24T10:00:00Z",
  "started_at": "2026-07-24T10:00:02Z",
  "finished_at": "2026-07-24T10:00:14Z",
  "duration_seconds": 12,
  "jobs_total": 2,
  "jobs_failed": 1,
  "jobs": [
    { "name": "build", "status": "failed" },
    { "name": "test", "status": "skipped" }
  ],
  "url": "https://ci.example.com/runs/a1b2c3d4"
}
```

Notes:

- **`status`** is the run's terminal state — one of `success`, `failed`,
  `cancelled`, `skipped`. **`reason`** explains a non-clean outcome: for a
  pre-execution failure (checkout/parse), a non-matching event, or a cancellation
  it is the run's recorded reason; for an ordinary job failure it is derived as
  `failed jobs: <names>` (which the `jobs` array also details). It is omitted for
  a clean success.
- **`repo_url`** has any embedded credentials (`https://user:token@host/…`)
  stripped before it is sent.
- **`started_at`** and **`duration_seconds`** are present only for runs that
  actually began executing. Pre-execution outcomes (checkout/parse failure, an
  event that didn't match `on:`) never started, so both are omitted — which, with
  the failures-only default, is the common case.
- **`jobs`** is summary-only: each job's name and status, plus `jobs_total` /
  `jobs_failed` counts. Step-level detail is not included (fetch it from
  `GET /api/runs/{id}` if you need it). The schema is additive — new fields may
  appear, so tolerate unknown keys.
- **`url`** is present only when `base_url` is set.

## Delivery semantics

- **Best-effort, never blocking.** Notifications are dispatched after the run is
  recorded, on their own goroutines. A slow, failing (`>= 300`), or unreachable
  endpoint is logged and **never fails or delays a run**.
- **Single attempt.** There is no retry or backoff; a failed delivery is dropped
  (and logged with the URL credentials redacted). If you need at-least-once
  delivery, put a queue in front of Janus.
- **Per-delivery timeout.** Each POST is bounded (10s) so a hung endpoint cannot
  pin resources.
- **Bounded per target.** Concurrent deliveries are capped per target; when a
  single endpoint has too many in flight (e.g. it is slow or unreachable),
  further deliveries to *that* target are dropped and logged. The cap is
  per-target, so a slow endpoint never blocks or starves the others.
- **Drained on shutdown.** On `SIGINT`/`SIGTERM`, after in-flight runs settle,
  Janus waits briefly for pending deliveries to flush before exiting, so a run
  that finishes just before shutdown still gets to notify.
- **Daemon-only.** Local `janus run` executes directly and does **not** notify.
  Crash-recovery bookkeeping (runs marked `cancelled` at startup after a hard
  kill) does not notify either — only live completions do.

## Receiving

The body is generic JSON, so any endpoint that accepts a JSON `POST` works. To
target a chat system that expects a specific message shape (Slack, Discord,
Teams, …), point Janus at a small adapter that receives this payload and reshapes
it — Janus keeps the transport to one generic webhook by design.

If you set a `secret`, verify the `Authorization: Bearer <secret>` header on the
receiver and reject deliveries without it.
