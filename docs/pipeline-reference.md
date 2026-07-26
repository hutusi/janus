# Pipeline reference

A Janus pipeline lives in the repository at **`.janus/ci.yml`**. It is a small,
GitHub-Actions-flavored YAML that describes *what runs and in what order*. It is
deliberately not a programming language: anything beyond the keys below is a
**validation error**, not a feature.

Validate a file locally:

```sh
janus validate .janus/ci.yml
```

## Full grammar

```yaml
name: ci                                  # required

on:                                       # required: at least one trigger
  push:
    branches: [main]                      # optional; omit/empty = all branches
    tags: ["v*"]                          # optional; tag pushes are opt-in
    paths: ["src/**", "go.mod"]           # optional; run only when these changed
  merge_request:                          # GitLab term; normalized internally
    branches-ignore: [wip]                # optional; every branch except these

concurrency:                              # optional; serialize runs that share a group
  group: deploy-${{ branch }}             # optional; default: <name>-<branch, tag or ref>
  cancel-in-progress: true                # optional; default false = queue

env:                                      # optional; workflow-wide variables
  CI: "true"

jobs:                                     # required: at least one job
  build:
    working-directory: ./app              # optional; default dir for this job's steps
    env:                                  # optional; job-level variables
      STAGE: build
    steps:
      - run: npm ci                       # required: the command
        shell: sh                         # optional; default: /bin/sh (unix), cmd (Windows)
        working-directory: ./app          # optional; relative to the workspace
        env:                              # optional; step-level variables
          NODE_ENV: production
  test:
    needs: [build]                        # optional; DAG dependencies
    steps:
      - run: npm test
  deploy:
    needs: [test]
    branches: [main]                      # optional; job runs only on these branches
    steps:
      - run: ./deploy.sh
  docs:
    paths-ignore: ["**.go"]               # optional; skip when only these changed
    steps:
      - run: ./build-docs.sh
```

### Keys

| Key                        | Where            | Required | Meaning |
|----------------------------|------------------|----------|---------|
| `name`                     | top level        | yes      | Workflow name. |
| `on.push.branches`         | top level        | —        | Run on push to these branches (empty = all). |
| `on.push.branches-ignore`  | top level        | —        | Run on push to every branch **except** these. Mutually exclusive with `branches`. |
| `on.push.tags`             | top level        | —        | Run on pushes of these [tags](#tag-filters) (glob patterns). Declaring it is what opts the workflow into tag pushes at all; mutually exclusive with `tags-ignore`. |
| `on.push.tags-ignore`      | top level        | —        | Run on every tag push **except** these. Also opts in to tag pushes; mutually exclusive with `tags`. |
| `on.push.paths`            | top level        | —        | Run only when a changed file matches a [path pattern](#path-filters). Push-only; mutually exclusive with `paths-ignore`. |
| `on.push.paths-ignore`     | top level        | —        | Skip the run when **every** changed file matches. Push-only; mutually exclusive with `paths`. |
| `on.merge_request.branches`| top level        | —        | Run on merge requests targeting these branches. |
| `on.merge_request.branches-ignore` | top level | —       | Run on merge requests except those **targeting** these branches. Mutually exclusive with `branches`. |
| `concurrency.group`        | top level        | —        | [Concurrency group](#concurrency-groups) template; tokens limited to `branch`, `tag`, `ref`, `event`, `env.NAME`. Empty/omitted = `<name>-<branch, tag or ref>`. |
| `concurrency.cancel-in-progress` | top level  | —        | `true`: a newer run in the group also cancels the one currently executing. Default `false` (queue). |
| `env`                      | top / job / step | —        | Environment variables, merged in that order. |
| `jobs.<id>`                | top level        | yes      | A job; the map key is the job name (letters, digits, `-`, `_` only). |
| `jobs.<id>.needs`          | job              | —        | Names of jobs that must succeed first (forms a DAG). |
| `jobs.<id>.branches`       | job              | —        | Run this job only on these branches; elsewhere it is recorded `skipped`. |
| `jobs.<id>.branches-ignore`| job              | —        | Run this job on every branch **except** these. Mutually exclusive with `branches`. |
| `jobs.<id>.paths`          | job              | —        | Run this job only when a changed file matches; otherwise it is recorded `skipped` (and so are jobs that `needs` it). See [path filters](#path-filters). |
| `jobs.<id>.paths-ignore`   | job              | —        | Skip this job when **every** changed file matches. Mutually exclusive with `paths`. |
| `jobs.<id>.working-directory` | job           | —        | Default directory (relative to the workspace) for the job's steps. |
| `jobs.<id>.steps[].run`    | step             | yes      | Command to run on the host via the step shell. |
| `jobs.<id>.steps[].shell`  | step             | —        | Shell for `run`: `sh`/`bash`/`cmd`/`powershell`/`pwsh`. Default: `/bin/sh` (unix), `cmd` (Windows). |
| `jobs.<id>.steps[].working-directory` | step  | —        | Directory (relative to the workspace) to run in; overrides the job default (`.` returns to the workspace root). |

At least one of `on.push` / `on.merge_request` must be present. Branch names in
both lists are **exact string matches** (no globs). A trigger may declare
`branches` (allowlist) or `branches-ignore` (denylist), but not both. Tag names
are the one exception to exact matching — see [tag filters](#tag-filters).

Each step runs in a fresh shell — a `cd` inside one step never affects the
next. The working directory is declarative instead: the job's
`working-directory` is the default for its steps, a step's own value overrides
it, and both are resolved relative to the workspace at run time (a path
escaping the workspace fails the step; an absolute path is anchored under the
workspace, not taken literally). The directory must exist in the checkout — a
missing one fails the step with the reason written to its log
(`janus: working-directory: … no such file or directory`); create it in an
earlier step if the repo doesn't contain it.

### Job-level branch filters

Jobs accept the same `branches` / `branches-ignore` keys (and the same
exact-match, one-or-the-other rules) as `on:` filters, matched against the
event's branch **regardless of event kind** — including manual triggers, whose
`branch` field decides what runs (`janus run --branch main`, or the API's
`"branch"` field; with no branch, jobs with a `branches` allowlist skip).
Where `on:` decides whether the *workflow* runs at all, a job filter shapes
*what runs within it*: a non-matching job is recorded `skipped` on the run —
visible in the dashboard and API — and a job that `needs` a branch-skipped job
is skipped too (its dependency never ran). If every job is filtered out, the
run itself finishes `skipped` with a reason. This is the supported way to run
one pipeline with a branch-dependent tail (build everywhere, deploy on `main`)
— see [examples/ci.yml](../examples/ci.yml); there is still no `if:` or
expression language.

### Tag filters

`tags` / `tags-ignore` on `on.push` run the workflow when a **tag** is pushed —
the release pipeline: build, publish, and announce when `v1.4.0` appears.

```yaml
name: release
on:
  push:
    tags: ["v*"]
jobs:
  publish:
    steps:
      - run: ./build.sh && ./publish.sh ${{ tag }}
```

**Tag pushes are opt-in.** A workflow that does not declare `tags` or
`tags-ignore` never runs on one, even with a bare `on: push:` and no filters at
all. This is a deliberate departure from GitHub Actions, where `on: push` with
no filters fires on tags as well. Janus runs jobs as unsandboxed host processes,
so a Janus upgrade must not silently start executing pipelines against refs they
have never seen; declaring the key is the opt-in, and it errs toward not running
code. Conversely, a workflow that declares **only** `tags` still runs on branch
pushes — add `branches` to narrow that, or keep the release pipeline in its own
file (`.janus/release.yml`, selected per webhook with `?pipeline_path=`).

Unlike branch names, tag patterns are **globs** — the same syntax and the same
matcher as [path filters](#path-filters), matched against the tag name:

| Pattern | Matches | Not |
|---------|---------|-----|
| `v1.0.0` | exactly `v1.0.0` | `v1.0.1` |
| `v*` | `v1.0.0`, `v2` | `release/v1` (`*` stays in one segment) |
| `v*.*.*` | `v1.0.0` | `v1.0` |
| `**` | every tag, including `release/v1` | — |

Branches match exactly and tags match by glob because the useful default
differs: a branch list names a handful of long-lived branches, while a release
pipeline is written once and must match every future version without editing the
YAML. `tags` is the allowlist form and `tags-ignore` the denylist (`tags-ignore:
["*-rc*"]` — every tag but the pre-releases); declaring both, or an empty list,
is a validation error.

A tag push carries no branch: `${{ branch }}` and `JANUS_BRANCH` are **empty**,
`${{ tag }}` and `JANUS_TAG` hold the tag name, and `${{ ref }}` is
`refs/tags/<tag>`. Two consequences worth knowing:

- A job-level `branches:` filter never matches on a tag push (there is no branch
  to match), so such a job is recorded `skipped`. Keep tag-only work in a
  workflow or job without a branch filter.
- [Path filters](#path-filters) are inert for tag pushes — a tag has no
  meaningful base to diff against — so a tag push runs path-filtered work, in
  keeping with the fail-open rule.

Tag **deletions** are ignored, like branch deletions. `on.merge_request` rejects
`tags`/`tags-ignore` at validation: a merge request has no tag.

### Path filters

`paths` / `paths-ignore` restrict work to pushes that actually touched the
matching files — the monorepo staples "skip CI for docs-only pushes" and
"build only the service whose subtree changed". At the trigger level
(`on.push.paths`) a non-matching push ends as a terminal `skipped` run with a
reason; at the job level the job is recorded `skipped` (and, transitively, so
is any job that `needs` it), exactly like the branch filters.

Patterns are a GitHub-Actions-style glob subset, matched against
slash-separated repo-relative paths, anchored at both ends:

| Pattern | Matches | Not |
|---------|---------|-----|
| `Makefile` | exactly `Makefile` | `sub/Makefile` |
| `*.go` | `main.go` | `cmd/main.go` (`*` stays in one segment) |
| `docs/**` | anything under `docs/` | `docs` itself |
| `**/*.go` | any `.go` file at any depth (incl. the root) | — |
| `cmd/*/main.go` | `cmd/janus/main.go` | `cmd/a/b/main.go` |

That is the whole syntax: `*` within a segment, `**` across segments
(including zero: `a/**/b` matches `a/b`), `?` for one character, everything
else literal. No character classes and no `!` negation — `paths` is the
allowlist form, `paths-ignore` the denylist form, and declaring both on one
trigger or job is a validation error (as is an empty list, which would match
nothing and silently skip every push).

Semantics: `paths` passes when **at least one** changed file matches **any**
pattern; `paths-ignore` passes unless **every** changed file is ignored. A
renamed file counts as a change to both its old and new path.

**Path filters apply to push events only** and **fail open**. The changed set
is `git diff` between the push's `before` commit and the checked-out head — a
pure tree diff, so the shallow (depth-1) checkout design is unchanged; the
base commit is fetched on demand, and only workflows that declare a path
filter pay that extra round-trip. Whenever the set cannot be determined — a
newly created branch (no base), a force-push whose base is gone, a server
that refuses fetch-by-SHA, a diff beyond 10 000 files — the filters are
ignored and the pipeline **runs**: a path filter must never wrongly skip CI.
Merge-request and manual events always run path-filtered work (an MR's
changed set needs a merge base, history the shallow checkout deliberately
avoids — so `on.merge_request` rejects `paths` at validation), and local
`janus run` ignores path filters entirely, like `concurrency:`.

### Concurrency groups

`concurrency:` serializes runs of the workflow that expand to the same
**group**. Per group, at most **one run executes and one waits**:

- A newer trigger **supersedes** the waiting run — it finishes `cancelled` with
  the reason `superseded by run <id>` and never executes. Pushing five commits
  in quick succession runs the first and the last, not all five. "Newer" is
  **trigger order**, even when checkouts finish out of order: an older trigger
  whose checkout completes late is itself cancelled as superseded rather than
  displacing (or outliving) a newer run with a stale commit.
- With `cancel-in-progress: true` the newer trigger also cancels the run
  currently *executing* (its processes are killed); with the default `false`
  the executing run finishes and the newest waiter starts.

`group:` is a template over a **subset** of the interpolation tokens: `branch`,
`ref`, `event`, and `env.NAME` (resolved against the workflow-level `env:`
only, so the group is known before any job runs). `sha` / `short_sha` are a
validation error here — every run would form its own group, silently disabling
the serialization the key exists to provide. With `group:` omitted or empty the
group is `<workflow name>-<branch>` (falling back to the tag, then the raw ref,
when the event has no branch), i.e. per-branch serialization of this workflow —
and per-tag serialization for [tag pushes](#tag-filters).

Groups are always **scoped to the triggering repository** — the same `group:`
string in two different repos never interferes. A bare `concurrency:` key with
nothing under it decodes as absent and does nothing; GitHub's string shorthand
(`concurrency: my-group`) is a validation error — use the mapping form. The
key applies to daemon-triggered runs (webhooks and the trigger API); a local
`janus run` executes immediately and ignores it.

### Step shell

Each step's `run` string is handed to a shell. With `shell:` omitted, the shell
is the host default — **`/bin/sh -c`** on unix, **`cmd /C`** on Windows. Override
it per step with a value from a closed set (anything else is a validation error):

| `shell:`     | Command |
|--------------|---------|
| `sh`         | `sh -c` |
| `bash`       | `bash -c` |
| `cmd`        | `cmd /C` |
| `powershell` | `powershell -NoProfile -NonInteractive -Command` |
| `pwsh`       | `pwsh -NoProfile -NonInteractive -Command` |

The chosen shell (or `git`) must be installed on the host. **Pipelines are not
portable across OSes:** a POSIX `run:` script (`set -e`, `test -d`, pipes) needs
`sh`/`bash`, which is not the Windows default — set `shell: sh` (e.g. with Git for
Windows on `PATH`), or write cmd/PowerShell for Windows hosts.

## Variable interpolation

Strings may reference a **closed set** of tokens with `${{ ... }}`:

| Token             | Value |
|-------------------|-------|
| `${{ env.NAME }}` | Environment variable `NAME` (merged workflow→job→step); undefined → empty. |
| `${{ ref }}`      | Full git ref, e.g. `refs/heads/main` or `refs/tags/v1.0.0`. |
| `${{ sha }}`      | Full commit SHA. |
| `${{ short_sha }}`| First 7 characters of the SHA. |
| `${{ branch }}`   | Branch name, e.g. `main`; empty on a tag push. |
| `${{ tag }}`      | Tag name, e.g. `v1.0.0`; empty on every event but a [tag push](#tag-filters). |
| `${{ event }}`    | Normalized event name: `push`, `merge_request`, or `manual`. |

Anything else inside `${{ ... }}` — operators, function calls, `secrets.*`,
`github.*`, `steps.*`, `matrix.*` — is a validation error. There is no
expression evaluation by design.

Interpolation is size-bounded: a step whose materialized command (>1 MiB),
working-directory (>4 KiB), any environment value (>64 KiB), or total
environment (>1 MiB) exceeds the limit **fails** rather than expanding
without bound — so a large value referenced many times can't exhaust memory.

## Environment precedence

For each step, variables merge in this order (later wins):

```
workflow env  →  job env  →  step env
```

Janus also injects a curated base for every step (e.g. `CI=true`, `PATH`,
`HOME`, and `JANUS_*` values for the ref/sha/branch). It does **not** pass the
Janus daemon's full environment into jobs, so daemon configuration is not
handed to builds via the environment. That is the extent of the guarantee:
jobs run as the same OS user as the daemon (no isolation), so anything that
user can read remains reachable — see the security model in the README.

## What is rejected (and why)

These produce a clear validation error rather than running:

- `branches` together with `branches-ignore` on the same trigger or job — pick
  an allowlist or a denylist.
- `paths` together with `paths-ignore` on the same trigger or job (same rule);
  `paths` on `on.merge_request` (push-only — see
  [path filters](#path-filters)); an empty `paths: []` (it would match nothing
  and silently skip every push).
- `tags` together with `tags-ignore` on the same trigger (same rule); either on
  `on.merge_request` (a merge request has no tag) or on a job (a job has no tag
  of its own — see [tag filters](#tag-filters)); an empty `tags: []`.
- `if:` / conditionals / expressions — no expression language. For the common
  "deploy only on main" case, use a declarative
  [job-level branch filter](#job-level-branch-filters) instead.
- `strategy:` / `matrix:` — no matrix builds.
- `uses:` / `with:` — no third-party actions; steps run host commands only.
- `runs-on:`, `container:`, `services:` — jobs run as host processes.
- `secrets:` — use host environment variables.
- `cache:`, `permissions:`, `outputs:`, step `id:`/`name:`; `concurrency:`
  anywhere but the top level, or as GitHub's string shorthand. Reusing checkouts
  and untracked build caches between runs is a server setting, not a pipeline
  key — see [persistent workspaces](configuration.md#persistent-workspaces).
- Job names outside `[A-Za-z0-9_-]` — the store derives log-file names from
  the job name, and a wider charset would let two jobs share one file.
- Absurd sizes — a pipeline file over 1 MiB (rejected at read, before parsing),
  more than 256 jobs, more than 256 steps in a job, a job name over 256
  characters, a step `run` over 64 KiB, or a `paths`/`paths-ignore` list with
  more than 50 patterns or a pattern over 256 characters. Generous limits that
  only reject the pathological, so per-run artifacts stay finite.
- A second YAML document (`---`) in the file — it would be silently ignored,
  hiding part of the file from validation.
- An unterminated `${{` — it would reach the shell verbatim.
- Any other unknown key (caught structurally), and cyclic / unknown `needs`.

## Execution model (summary)

- Jobs run as **host processes** (no container/VM). They run concurrently when
  their `needs` allow, and a job's steps run sequentially.
- Jobs excluded by a [branch filter](#job-level-branch-filters) — and jobs that
  `needs` one — are marked `skipped` up front and never launched; a skipped job
  is not a failure.
- A step fails on the first non-zero exit; that fails the job.
- On the first job failure the run is **fail-fast**: in-flight processes are
  cancelled and not-yet-started jobs are marked skipped. The run itself is
  `failed` — the run-level `cancelled` status means external interruption:
  daemon shutdown, Ctrl-C on `janus run`, a
  [concurrency-group](#concurrency-groups) supersede, or
  `POST /api/runs/{id}/cancel`.

See [architecture.md](architecture.md) for the full lifecycle. Runnable sample
pipelines live in [examples/](../examples/).
