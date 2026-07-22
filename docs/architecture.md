# Architecture

Janus is one Go binary, standard library throughout, with a single third-party
module (`gopkg.in/yaml.v3`). It compiles to a static binary
(`CGO_ENABLED=0`) with no runtime dependencies.

## Package map

```
cmd/janus            CLI: serve | run | validate; flag parsing, wiring, shutdown
internal/
  model              domain types: spec (Workflow/Job/Step), Event, runtime (Run/JobRun/StepRun)
  pipeline           YAML parse + strict validation + interpolation   (pure, no I/O)
  engine             DAG build, scheduler, host-process executor
  workspace          per-run shallow git checkout + cleanup, or in-place reuse
  provider           webhook providers (GitLab) -> normalized Event
  runner             checkout -> parse -> match -> execute coordinator
  store              run/log persistence: Memory and File
  server             HTTP API, webhook endpoint, read-only dashboard
```

Dependencies point inward: everything may import `model`; `pipeline` and the DAG
half of `engine` are pure and have the heaviest unit tests.

## Domain model: two layers

- **Spec** (`model.Workflow/Triggers/Job/Step`) — immutable, produced by
  `pipeline.Parse`. Jobs are a map keyed by name.
- **Runtime** (`model.Run/JobRun/StepRun` + `Status`) — mutable, persisted.
  `engine.NewRun` materializes a `Run` (one `JobRun` per job, one `StepRun` per
  step) from a workflow; the YAML is never re-parsed at runtime.

`model.Event` is the normalized trigger (push / merge_request / manual) that
every provider and the manual API produce, and is the source of
`${{ ref|sha|branch|event }}`.

## Pipeline parsing & strict rejection

`pipeline.Parse` decodes into raw structs with `yaml.Decoder.KnownFields(true)`,
so any key outside the supported set (`if`, `matrix`, `uses`, `with`, `runs-on`,
…) is a decode **error**, not silently ignored. Decode errors of the form
"field X not found" are rewritten into spec-aware messages. Semantic validation
then checks triggers, steps, and `needs` (unknown/self/duplicate), runs a
3-colour-DFS **cycle check**, and validates that every `${{ ... }}` placeholder
matches a closed whitelist regex — which is how expressions and unknown contexts
are rejected.

## Run lifecycle

```
trigger (webhook / manual API; the CLI has its own synchronous path)
  → runner.Trigger (synchronous — everything here is HTTP-mappable):
      allowlist check (403) · event-field caps (400) · pipeline_path (400)
      admission cap (503 busy)
      engine.NewPendingRun + store.SaveRun  → return run id (202 accepted;
                                              503 if the store rejects it)
  → async (bounded by --max-parallel-runs; the whole trigger — checkout,
    parse, pending queue — is capped at 4× that, beyond which the API
    sheds load with 503). The response never waits on git: a webhook
    platform's delivery timeout cannot race the checkout, and a client
    hangup cannot cancel it. Pre-execution outcomes land on the run as a
    terminal status + reason (failed: checkout/parse error; skipped:
    non-matching event) — every recorded run reaches a terminal state:
      workspace.Checkout  (shallow fetch of the SHA, detached checkout;
                           a ref-fallback checkout is verified against the
                           requested SHA — a moved ref fails the run rather
                           than silently executing a commit the run's
                           metadata does not identify)
      read + pipeline.Parse  (pipeline_path from the checkout; a manual
                              trigger's pipeline_path field overrides it)
      match event against on:  (manual always matches)
      engine.PopulateRun (workflow name + job tree) + store.UpdateRun
      engine.Execute:
        buildGraph (indegree + dependents, Kahn cycle guard)
        readiness-driven scheduler:
          jobs with indegree 0 launch; bounded by --max-parallel-jobs
          on completion, decrement dependents; newly-ready jobs launch
          fail-fast: first non-success cancels the run ctx and stops launching
        per job (executor): steps run sequentially via the step shell
                            (default /bin/sh on unix, cmd on Windows; or shell:)
          env merge (curated base → workflow → job → step), ${{...}} interpolation
          combined stdout+stderr streamed to store.LogWriter (+ CLI tee)
          process group per step; cancel/timeout kills the whole group
      store.UpdateRun on every status change
  → workspace cleaned up; non-terminal jobs/steps marked skipped
```

## Concurrency & safety

- During **execution**, all mutations to a `Run` go through `runState.update`,
  which holds a mutex and persists under it — so parallel job goroutines never
  race, and the store always serializes a consistent snapshot. *Before*
  execution the run needs no such serialization: it is owned exclusively by its
  single trigger goroutine (`runner.runTrigger` populates it and records
  pre-execution outcomes directly), and stores snapshot on write, so no other
  goroutine can observe a partial mutation. Tests run under `-race`.
- Two caps bound host load: `--max-parallel-runs` and `--max-parallel-jobs`.
- Every run executes under a **per-run cancellable context** (a
  `WithCancelCause` child of the runner's root context, so Shutdown still
  reaches everything). Its cancel func lives in a registry keyed by run ID from
  *before* the pending run is saved until the run settles — the invariant is
  that any non-terminal run fetchable from the store can be cancelled
  (`Runner.Cancel`, backing `POST /api/runs/{id}/cancel`, and the
  concurrency-group supersede path). Every pre-execution wait — the checkout,
  the run-slot queue — selects on this context, and `Execute` receives it, so
  one mechanism cancels a run at any stage. The human-readable cause
  ("cancelled via API", "superseded by run X") becomes the run's `Reason`; it
  is attached only *after* `Execute` returns (the run is single-owner again —
  writing it mid-flight would race `runState`), and cancellation is
  best-effort: a run whose last process exits before the cancel is observed
  still finishes on its own terms.
- **Concurrency groups** (a workflow's `concurrency:` key) are enforced by a
  runner-side registry keyed by `(repo URL, expanded group)`: per group **at
  most one run executes and at most one waits** — a newer arrival supersedes
  the waiter (cancelled via its per-run context, reason
  `superseded by run <id>`), and with `cancel-in-progress` it cancels the
  executing member too. **"Newer" means trigger order**: a monotonic sequence
  is stamped synchronously in `Trigger`, and a per-group high-water mark (the
  newest member ever admitted) refuses older arrivals — necessary because
  members reach the registry only after their checkout, and checkouts finish
  in any order, so an older trigger arriving late must not supersede, kill,
  or outlive a newer run with a stale commit. The mark survives the group
  emptying, but only as long as it can matter: a stale arrival is by
  definition an admitted older trigger that has not yet **resolved** —
  entered its one group, turned out ungrouped, or failed pre-enter — so each
  trigger retires from the accounting at resolution (grouped ones inside
  `enter` itself), and marks below the oldest unresolved trigger are swept.
  Since the pre-enter phase is deadline-bounded (the checkout timeout), a
  mark outlives its creation by at most ~one checkout window regardless of
  how long runs execute — registry memory is never pinned by a hung step or
  by how many branches (groups) the daemon has ever seen. Sequence hand-out
  and enrollment happen under one lock (a sweep between the two steps could
  drop a mark a just-admitted older trigger still needs), and inside `enter`
  the stale-gate check precedes retirement — the stale arrival's own
  enrollment is what has kept the refusing mark alive. A run cancelled
  before it enters resolves without superseding anyone. One conscious
  trade-off: under `workspace_strategy: persistent`, a grouped run queued
  behind its group holds its repo's persistent-workspace lock for the wait,
  so same-repo triggers fall back to fresh clones meanwhile — slower,
  never blocking. The
  group gate is ordered *before* the global run semaphore, so a run waiting
  for its group holds no run slot and cannot starve other repos; after
  acquiring a slot the member re-checks its membership under the registry
  lock (a supersede can land in between). The newcomer's turn begins only
  when the previous member has fully unwound (processes killed), so "one
  running" holds even mid-handover. Empty group entries are deleted; the
  registry is in-memory only (startup reconciliation already settles
  orphans). The group is only knowable *after* checkout+parse — the pipeline
  lives in the repo — so admission (`ErrBusy`/503) is unaffected and a
  superseded run still consumed an admission slot for its lifetime.
- Checkout git subprocesses run non-interactively (`GIT_TERMINAL_PROMPT=0`,
  ssh `BatchMode=yes` + bounded connect + keepalive unless the operator sets
  `GIT_SSH_COMMAND`/`GIT_SSH`) — a credential or host-key prompt, an
  unreachable host, or a stalled transfer would otherwise pin an admission
  slot (and its pending run) until the 10-minute checkout deadline instead of
  failing the run in seconds with a named reason.
- Workspaces are per-run and removed on completion; a startup **sweep** clears
  orphans left by a crash, and runs left `pending`/`running` by a crash are
  marked `cancelled` at startup (their goroutines died with the old process —
  nothing would ever finish them). Under `workspace_strategy: persistent`, each repo
  instead gets one reusable `persist-*` directory, updated by fetch +
  hard-reset and serialized by a per-repo **try-lock** — a concurrent trigger
  for the same repo falls back to a fresh per-run dir rather than blocking.
  `persist-*` dirs deliberately survive restarts and the sweep.
- **No isolation:** jobs are host processes and can do anything the `janus` user
  can. Run as a dedicated unprivileged user. Containers are out of scope.

## Extending: new providers

Implement `provider.Provider` (`Name`, `Verify`, `Parse → *model.Event`) and
register it with `server.WithProvider`. The rest of the system — matching,
checkout, scheduling — is provider-agnostic. GitHub/Gitea would slot in here.
