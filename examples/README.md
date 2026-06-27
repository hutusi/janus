# Example pipelines

Two ready-to-copy [Janus](../README.md) pipelines for a typical npm / static-site
project. See [docs/pipeline-reference.md](../docs/pipeline-reference.md) for the full
grammar.

| File | What it does |
|------|--------------|
| [`build.yml`](build.yml) | On every update to `master`, builds the project and verifies the `out/` output. |
| [`release.yml`](release.yml) | The same build, then publishes `out/` to a separate "pages" repository. |

## How Janus uses these

Janus loads **one** pipeline file per repository per trigger — by default
`.janus/ci.yml` (configurable with `--pipeline-path`). So pick the model that fits:

- **Build on CI, release on demand** — copy `build.yml` to `.janus/ci.yml` so a webhook
  builds every master update, and run the release explicitly when you want to publish:

  ```sh
  janus run --pipeline-path .janus/release.yml .
  ```

- **Build + deploy on every master merge** — use `release.yml` as your `.janus/ci.yml`.

## Trigger: why `push`, not `merge_request`

Both files trigger on a **push to `master`**. When a merge request is merged, GitLab
sends a Push Hook to the target branch — Janus runs on that. Janus deliberately
*ignores* the MR `merge` action (it only acts on `open`/`reopen`/`update`), so
`on: merge_request` would **not** fire when an MR lands.

Janus can't distinguish a merge-commit push from a direct push to `master`. If you want
runs only for merged MRs, protect `master` so it can't be pushed to directly.

## "Update master to the latest"

Janus checks out the triggering commit as a **shallow (`--depth 1`), detached HEAD** —
which already *is* the latest `master` tip for a push event. `git checkout master` or
`git pull` would fail (that branch isn't in the shallow clone), so the build step uses:

```sh
git fetch --depth 1 origin master
git reset --hard FETCH_HEAD
```

Each step runs from the workspace root via `/bin/sh -c`. The filesystem persists across
steps, but shell state (current directory, variables) does **not** — that's why the
deploy steps use `git -C .pages-repo …` instead of `cd`.

## Releasing to the pages repo (SSH)

Janus forwards only `PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`, `TMPDIR` to steps — no
secrets, and not `SSH_AUTH_SOCK`. So configure git auth on the **host**, for the user
the Janus service runs as (see [docs/deployment.md](../docs/deployment.md)):

1. Generate a passphraseless SSH **deploy key** and add its public half to the pages
   repo with **write** access.
2. Place the private key in the Janus user's `~/.ssh/` and add the host to
   `~/.ssh/known_hosts` (a passphraseless key works without an agent).
3. Set the knobs in `release.yml`'s `env:` — `PAGES_REPO_URL` (an SSH `git@…` URL),
   `PAGES_BRANCH` (must already exist), and the commit identity. These are not secrets.

The deploy job mirrors `out/` into the pages repo (overwriting, including deletions),
commits only if something changed, and pushes.
