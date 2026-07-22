# Example pipelines

Two ready-to-copy [Janus](../README.md) pipelines for a typical npm / static-site
project. See [docs/pipeline-reference.md](../docs/pipeline-reference.md) for the full
grammar.

| File | What it does |
|------|--------------|
| [`build.yml`](build.yml) | On every push to any branch **except** `master`/`main`, builds the pushed branch and verifies the `out/` output. |
| [`release.yml`](release.yml) | On every update to `master`/`main`, the same build, then publishes `out/` to a separate "pages" repository. |

## How Janus uses these

Janus loads **one** pipeline file per trigger — by default `.janus/ci.yml`
(configurable with `--pipeline-path`; a trigger may name another committed file).
There is deliberately no `if:` or per-job branch condition in the pipeline
grammar — branch routing is done by *pairing files with complementary `on:`
filters*, one trigger each. Pick the model that fits:

- **Branch routing: build every branch, release master/main** — the two files
  are complementary (`branches-ignore: [master, main]` vs
  `branches: [master, main]`). Commit build.yml's content as `.janus/ci.yml`
  and release.yml as `.janus/release.yml`, then register **two webhooks** on
  the project (GitLab allows any number, same URL + secret):

  - `https://janus.example.com/webhooks/gitlab` — runs `.janus/ci.yml`
  - `https://janus.example.com/webhooks/gitlab?pipeline_path=release.yml` —
    runs `.janus/release.yml`

  Every push is delivered to both hooks; each file's `on:` filter decides which
  one executes. A feature push executes the build workflow while the release
  delivery records a `skipped` run (with the non-match reason on the run); a
  `master`/`main` push does the reverse. Both deliveries always answer
  `202 {"status":"accepted"}`.

- **Build every branch on CI, release on demand** — commit only the build
  pipeline as `.janus/ci.yml` and publish explicitly when you want to:

  ```sh
  janus run --file .janus/release.yml .
  ```

  or keep both files in `.janus/` and trigger the release through the API's
  `pipeline_path` override (`{"pipeline_path": "release.yml", ...}`).

- **Build + deploy on every master/main merge only** — use `release.yml` as
  your `.janus/ci.yml`; feature pushes then record `skipped` runs.

## Triggers: a denylist for CI, `push` for releases

`build.yml` uses `branches-ignore: [master, main]` — an exact-match **denylist**, so
every other pushed branch builds with no list to maintain. GitLab sends a Push Hook
per pushed branch; Janus matches the branch against the filter and builds that branch.

`release.yml` fires on a **push to `master` or `main`**. When a merge request is
merged, GitLab sends a Push Hook to the target branch — Janus runs on that. Janus
deliberately *ignores* the MR `merge` action (it only acts on
`open`/`reopen`/`update`), so `on: merge_request` would **not** fire when an MR
lands. Janus can't distinguish a merge-commit push from a direct push — if you want
runs only for merged MRs, protect the branch so it can't be pushed to directly.

## "Update the branch to the latest"

Janus checks out the triggering commit as a **shallow (`--depth 1`), detached HEAD** —
which already *is* the latest tip of the pushed branch for a push event.
`git checkout <branch>` or `git pull` would fail (that branch isn't in the shallow
clone), so the build step uses:

```sh
git fetch --depth 1 origin "${{ branch }}"
git reset --hard FETCH_HEAD
```

Each step runs from the workspace root via the step shell — `/bin/sh -c` by default
on unix (these examples are POSIX; on Windows set `shell: sh`, or write cmd/PowerShell).
The filesystem persists across steps, but shell state (current directory, variables)
does **not** — that's why the deploy steps use `git -C .pages-repo …` instead of `cd`.

## Releasing to the pages repo (SSH)

Janus forwards only `PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`, `TMPDIR` to steps — no
secrets, and not `SSH_AUTH_SOCK`. So configure git auth on the **host**, for the user
the Janus service runs as (see [docs/deployment.md](../docs/deployment.md)):

1. Generate a passphraseless SSH **deploy key** and add its public half to the pages
   repo with **write** access.
2. Place the private key in the Janus user's `~/.ssh/` and add the host to
   `~/.ssh/known_hosts` (a passphraseless key works without an agent).
3. Set the knobs in `release.yml`'s `env:` — `PAGES_REPO_URL` (an SSH `git@…` URL),
   `PAGES_BRANCH` (must already exist), and the commit identity (`GIT_USER_NAME` /
   `GIT_USER_EMAIL`). These are not secrets.

The deploy job mirrors `out/` into the pages repo (overwriting, including deletions),
commits only if something changed, and pushes.
