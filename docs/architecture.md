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
