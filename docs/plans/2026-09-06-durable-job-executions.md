# Durable job executions

Date: 2026-09-06
Scope: [issue #160](https://github.com/labstack/onebox/issues/160).

Onebox already owns scheduled container execution, deployment coordination,
release leases, and host run records. Extend that boundary with recoverable
executions and optional linear steps. Applications retain ownership of domain
checkpoints, transaction boundaries, and effect deduplication.

## Contract

- Opt-in `execution` on native, scheduled, manual jobs declaring
  `data_effect: none`. Existing migration, destructive-effect, unknown-effect,
  and release-phase job paths retain their policy gates.
- `execution.retention` defaults to `168h`, with a maximum of `30d` from creation.
  Empty `steps` uses the existing job command as the implicit `main` step.
- Explicit steps use `id`, argv `command`, declared `outputs`, earlier-step
  `inputs` references, and optional `retry`. No branching, parallel graph,
  compensation engine, or expression evaluator.
- Persist resolved original inputs, workflow definition, release and image
  identities, compatibility evidence, step states, validated outputs, and
  activation/attempt history. Persist no credential values.
- A step's success and outputs become durable in one checkpoint. Repeating an
  attempt after uncertain completion is possible; execution and step IDs remain
  stable, and attempt IDs change. This is at-least-once orchestration.
- Resume is explicit and uses original inputs. Timer firings create new
  executions; reboot does not automatically resume interrupted executions.
- Timeout, overlap protection, deployment coordination, and notifications apply
  to an activation spanning all remaining steps and backoff. A resume starts a
  new bounded activation; the creation-based retention deadline does not move.

## Storage decision: bounded atomic files

Use one protected JSON checkpoint per execution on the managed host, under
`<app-dir>/schedule/executions`. A standard-library Python helper runs on demand;
the host requires Python 3.8+ at `/usr/bin/python3`. Systemd remains the scheduler
and process supervisor. There is no additional daemon or database service.

SQLite was considered for transaction boundaries and indexed inspection. This
first model has one aggregate per execution, a bounded linear history, and
serialized updates. Whole-record replacement makes the important transaction
explicit: step state and outputs cannot commit separately. Files also keep the
host runtime dependency to Python's standard library and avoid a database
schema/migration and journal-management surface. SQLite remains a reasonable
future choice if query scale or cross-record transactions justify it; a process
restart alone is not a reason to need a database.

The write protocol is a temporary file in the same directory, flush and file
`fsync`, atomic replacement, and directory `fsync`. A store lock serializes
updates. Reads reject unsupported/corrupt evidence and oversized records rather
than interpreting them as completed work. The execution record is limited to
4 MiB and 100 activations. Step/output limits prevent unbounded checkpoint
payloads, and original inputs remain subject to existing declaration validation.

Publishing a new execution's durable release reference happens while the
scheduler's retention rendezvous and live release lease protect the original
release. Release cleanup reads durable references as well as live leases.
Successful or abandoned executions no longer pin releases; expired inactive
executions stop pinning. A record still claiming running work remains protected
even after expiry: a deadline cannot prove that a worker stopped. Corrupt state
must fail cleanup closed.

Host disk durability is the boundary. This does not replicate checkpoints or
recover a lost managed host. Terminal evidence remains inspectable; the resume
deadline is not a promise to delete execution records or journal entries.

## Recovery and compatibility

Before retrying interrupted work, establish that no prior job container is
running. Hold the existing per-job/deployment coordination locks, recheck the
resume request on the host, and claim the activation in the durable record.
Inspection can derive interrupted status from systemd evidence without mutating
the saved checkpoint. Uncertain worker ownership must not authorize a retry.

The initial compatibility rule is deliberately conservative: the original
release must still be current, its installed workflow definition must match,
and its image must remain available with the original content identity. Compare
release-file evidence and managed-service container IDs, image identities, and
start times. Also compare Onebox's data-change generation marker, which changes
when a coordinated operation may change data. Retaining old files alone cannot
prove that today's services or schemas can use them.

A service restart or reboot may refuse resume. Current credential resolution is
used for each attempt; credentials are not saved in checkpoint state. Arbitrary
external schema/data changes cannot be inferred from container evidence, so the
application still needs checks for compatibility it can understand. No automatic
cross-release adaptation or application schema migration is included.

## Public interface

```yaml
execution:
  retention: 168h
  steps:
    - id: sync
      command: ["./catalog", "sync"]
      outputs: [RELEASE]
    - id: index
      command: ["./catalog", "index"]
      inputs: {RELEASE_ID: "sync.RELEASE"}
      retry: {attempts: 3}
```

Commands: `ob execution list`, `inspect <id>`, `resume <id> [--wait]`, and
`abandon <id>`. Mutations use Onebox's operation/journal boundary. Inspect returns
saved inputs, steps, attempts, release, and resume eligibility. Logs remain in
journald: use an activation's invocation ID with `ob schedule logs <job> --run`.

The application writes exactly its declared keys as a JSON string map to
`ONEBOX_OUTPUT_FILE`: each value is at most 4096 UTF-8 bytes without NUL; both
the supplied file and normalized JSON must fit in 16384 bytes. Store references
to large artifacts, not the artifacts themselves. Inputs and outputs are
non-secret operational metadata. Onebox exposes stable `ONEBOX_EXECUTION_ID`
and `ONEBOX_STEP_ID`, plus a unique `ONEBOX_ATTEMPT_ID` for diagnostics.

Naming follows familiar conventions without importing their execution models.
GitHub Actions identifies output-producing steps with `id` and makes outputs
available to later steps; Onebox uses a simple `stepID.OUTPUT` reference rather
than its expression language. See the official [workflow commands reference](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-commands#setting-an-output-parameter).
Temporal uses retention terminology for how long execution history remains in
persistence; Onebox's `retention` specifically bounds resumability and release
protection from creation, and does not promise the same lifecycle semantics.
See Temporal's official [glossary](https://github.com/temporalio/documentation/blob/main/docs/glossary.md).

## Validation and release evidence

Required checks cover invalid workflow declarations, retry-budget inheritance,
atomic output/success commits, failed-step-only retries, interruption inspection,
stable IDs, original-input preservation, active-worker refusal, compatibility
changes, expiry/abandonment, and release cleanup with corrupt or uncertain state.
Exercise output-size/type violations and crash boundaries around file flush,
replacement, and directory synchronization. Keep ordinary scheduled jobs on
their existing path when execution is not enabled.

Tests using a simulated process or filesystem are useful evidence for protocol
transitions, but do not establish real-host power-loss recovery. Qualify the
systemd/Docker path on a supported Linux host, including runner termination,
reboot, and concurrent deployment/cleanup, before claiming those guarantees as
tested. This design note records requirements and decisions, not a claim that
real-host qualification has passed.

Implementation verification: the targeted Linux server test now exercises the
public CLI through SSH, deployment and schedule installation, failure/output
handoff, stable identities, explicit resume, SIGKILL interruption, active-run
refusal, deployment-lock exclusion, and reclaiming stopped legacy Compose job
containers using matching ownership labels. Pre-attempt cleanup never forces
container removal. Unit tests cover atomic replacement
failure, invalid outputs, service/data-generation changes, and corrupt retention
evidence. `just check` and Go lint pass. A full host reboot or physical power-cut
test has not been run; process interruption is not presented as that evidence.

Expired non-running snapshots are pruned when a new execution is created.
Unknown running snapshots require explicit abandonment before expiry can remove
them. Arbitrary audited exec invalidates compatibility before running, just as
jobs with non-none data effects do.
