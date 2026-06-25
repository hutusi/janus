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
  merge_request:                          # GitLab term; normalized internally
    branches: [main]

env:                                      # optional; workflow-wide variables
  CI: "true"

jobs:                                     # required: at least one job
  build:
    env:                                  # optional; job-level variables
      STAGE: build
    steps:
      - run: npm ci                       # required: the command (via /bin/sh -c)
        working-directory: ./app          # optional; relative to the workspace
        env:                              # optional; step-level variables
          NODE_ENV: production
  test:
    needs: [build]                        # optional; DAG dependencies
    steps:
      - run: npm test
```

### Keys

| Key                        | Where            | Required | Meaning |
|----------------------------|------------------|----------|---------|
| `name`                     | top level        | yes      | Workflow name. |
| `on.push.branches`         | top level        | —        | Run on push to these branches (empty = all). |
| `on.merge_request.branches`| top level        | —        | Run on merge requests targeting these branches. |
| `env`                      | top / job / step | —        | Environment variables, merged in that order. |
| `jobs.<id>`                | top level        | yes      | A job; the map key is the job name. |
| `jobs.<id>.needs`          | job              | —        | Names of jobs that must succeed first (forms a DAG). |
| `jobs.<id>.steps[].run`    | step             | yes      | Command to run on the host via `/bin/sh -c`. |
| `jobs.<id>.steps[].working-directory` | step  | —        | Directory (relative to the workspace) to run in. |

At least one of `on.push` / `on.merge_request` must be present.

## Variable interpolation

Strings may reference a **closed set** of tokens with `${{ ... }}`:

| Token             | Value |
|-------------------|-------|
| `${{ env.NAME }}` | Environment variable `NAME` (merged workflow→job→step); undefined → empty. |
| `${{ ref }}`      | Full git ref, e.g. `refs/heads/main`. |
| `${{ sha }}`      | Full commit SHA. |
| `${{ short_sha }}`| First 7 characters of the SHA. |
| `${{ branch }}`   | Branch name, e.g. `main`. |
| `${{ event }}`    | Normalized event name: `push`, `merge_request`, or `manual`. |

Anything else inside `${{ ... }}` — operators, function calls, `secrets.*`,
`github.*`, `steps.*`, `matrix.*` — is a validation error. There is no
expression evaluation by design.

## Environment precedence

For each step, variables merge in this order (later wins):

```
workflow env  →  job env  →  step env
```

Janus also injects a curated base for every step (e.g. `CI=true`, `PATH`,
`HOME`, and `JANUS_*` values for the ref/sha/branch). It does **not** pass the
Janus daemon's full environment into jobs, so daemon configuration never leaks
into builds.

## What is rejected (and why)

These produce a clear validation error rather than running:

- `if:` / conditionals / expressions — no expression language.
- `strategy:` / `matrix:` — no matrix builds.
- `uses:` / `with:` — no third-party actions; steps run host commands only.
- `runs-on:`, `container:`, `services:` — jobs run as host processes.
- `secrets:` — use host environment variables.
- `cache:`, `permissions:`, `concurrency:`, `outputs:`, step `id:`/`name:`/`shell:`.
- Any other unknown key (caught structurally), and cyclic / unknown `needs`.

## Execution model (summary)

- Jobs run as **host processes** (no container/VM). They run concurrently when
  their `needs` allow, and a job's steps run sequentially.
- A step fails on the first non-zero exit; that fails the job.
- On the first job failure the run is **fail-fast**: in-flight processes are
  cancelled and not-yet-started jobs are marked skipped.

See [architecture.md](architecture.md) for the full lifecycle.
