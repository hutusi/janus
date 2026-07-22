# Example pipeline

A ready-to-copy [Janus](../README.md) pipeline for a typical npm / static-site
project. See [docs/pipeline-reference.md](../docs/pipeline-reference.md) for the full
grammar.

| File | What it does |
|------|--------------|
| [`ci.yml`](ci.yml) | On every push: builds the pushed branch and verifies the `out/` output; on `master`/`main` additionally publishes `out/` to a separate "pages" repository. |

## How the branch routing works

`ci.yml` triggers on **every** push (`on: push` with no filter). The `build` job
always runs; the `deploy` job carries a **job-level branch filter**:

```yaml
deploy:
  needs: [build]
  branches: [master, main]
```

On a feature branch, `deploy` is recorded as `skipped` (visible on the run in
the dashboard and API) while `build` executes; on `master`/`main` both run.
Job filters use the same `branches` / `branches-ignore` syntax and matching as
`on:` filters, and a job that `needs` a branch-skipped job is skipped too.
There is deliberately no `if:` or expression language — routing stays
declarative.

Copy the file to `.janus/ci.yml` in your repository and it works with the
default configuration; one webhook is enough.

To force the release path by hand (the branch decides what runs):

```sh
janus run --branch main .
```

or through the API: `POST /api/trigger` with `{"branch": "main", ...}`.

For pipelines that are genuinely separate (say, a nightly maintenance file that
should not run on push at all), commit them beside `ci.yml` and select them
per trigger: a second webhook with `?pipeline_path=nightly.yml`, or the API's
`pipeline_path` field.

## Releases: `push`, not `merge_request`

The release fires on a **push to `master` or `main`**. When a merge request is
merged, GitLab sends a Push Hook to the target branch — Janus runs on that.
Janus deliberately *ignores* the MR `merge` action (it only acts on
`open`/`reopen`/`update`), so `on: merge_request` would **not** fire when an MR
lands. Janus can't distinguish a merge-commit push from a direct push — if you
want runs only for merged MRs, protect the branch so it can't be pushed to
directly.

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
on unix (the example is POSIX; on Windows set `shell: sh`, or write cmd/PowerShell).
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
3. Set the knobs in `ci.yml`'s `env:` — `PAGES_REPO_URL` (an SSH `git@…` URL),
   `PAGES_BRANCH` (must already exist), and the commit identity (`GIT_USER_NAME` /
   `GIT_USER_EMAIL`). These are not secrets.

The deploy job mirrors `out/` into the pages repo (overwriting, including deletions),
commits only if something changed, and pushes.
