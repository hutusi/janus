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
    branches-ignore: [wip]                # optional; every branch except these

env:                                      # optional; workflow-wide variables
  CI: "true"

jobs:                                     # required: at least one job
  build:
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
```

### Keys

| Key                        | Where            | Required | Meaning |
|----------------------------|------------------|----------|---------|
| `name`                     | top level        | yes      | Workflow name. |
| `on.push.branches`         | top level        | —        | Run on push to these branches (empty = all). |
| `on.push.branches-ignore`  | top level        | —        | Run on push to every branch **except** these. Mutually exclusive with `branches`. |
| `on.merge_request.branches`| top level        | —        | Run on merge requests targeting these branches. |
| `on.merge_request.branches-ignore` | top level | —       | Run on merge requests except those **targeting** these branches. Mutually exclusive with `branches`. |
| `env`                      | top / job / step | —        | Environment variables, merged in that order. |
| `jobs.<id>`                | top level        | yes      | A job; the map key is the job name (letters, digits, `-`, `_` only). |
| `jobs.<id>.needs`          | job              | —        | Names of jobs that must succeed first (forms a DAG). |
| `jobs.<id>.steps[].run`    | step             | yes      | Command to run on the host via the step shell. |
| `jobs.<id>.steps[].shell`  | step             | —        | Shell for `run`: `sh`/`bash`/`cmd`/`powershell`/`pwsh`. Default: `/bin/sh` (unix), `cmd` (Windows). |
| `jobs.<id>.steps[].working-directory` | step  | —        | Directory (relative to the workspace) to run in. |

At least one of `on.push` / `on.merge_request` must be present. Branch names in
both lists are **exact string matches** (no globs). A trigger may declare
`branches` (allowlist) or `branches-ignore` (denylist), but not both.

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
Janus daemon's full environment into jobs, so daemon configuration is not
handed to builds via the environment. That is the extent of the guarantee:
jobs run as the same OS user as the daemon (no isolation), so anything that
user can read remains reachable — see the security model in the README.

## What is rejected (and why)

These produce a clear validation error rather than running:

- `branches` together with `branches-ignore` on the same trigger — pick an
  allowlist or a denylist.
- `if:` / conditionals / expressions — no expression language.
- `strategy:` / `matrix:` — no matrix builds.
- `uses:` / `with:` — no third-party actions; steps run host commands only.
- `runs-on:`, `container:`, `services:` — jobs run as host processes.
- `secrets:` — use host environment variables.
- `cache:`, `permissions:`, `concurrency:`, `outputs:`, step `id:`/`name:`.
- Job names outside `[A-Za-z0-9_-]` — the store derives log-file names from
  the job name, and a wider charset would let two jobs share one file.
- A second YAML document (`---`) in the file — it would be silently ignored,
  hiding part of the file from validation.
- An unterminated `${{` — it would reach the shell verbatim.
- Any other unknown key (caught structurally), and cyclic / unknown `needs`.

## Execution model (summary)

- Jobs run as **host processes** (no container/VM). They run concurrently when
  their `needs` allow, and a job's steps run sequentially.
- A step fails on the first non-zero exit; that fails the job.
- On the first job failure the run is **fail-fast**: in-flight processes are
  cancelled and not-yet-started jobs are marked skipped. The run itself is
  `failed` — the run-level `cancelled` status is reserved for external
  interruption (daemon shutdown, Ctrl-C on `janus run`).

See [architecture.md](architecture.md) for the full lifecycle. Runnable sample
pipelines live in [examples/](../examples/).
