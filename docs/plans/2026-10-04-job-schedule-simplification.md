# Job and schedule simplification

Date: 2026-10-04
Status: feature reduction implemented; validation results below.
Decision: [Jobs run commands; applications own workflows](../decisions/2026-10-04-job-schedule-simplification.md).

## Conclusion

The overengineering was the durable workflow layer on top of an already useful
job scheduler. Remove that feature, not just rearrange its implementation.
Onebox should run a command at a specified time and make its outcome easy to
inspect. The application should decide how its work progresses and recovers.

The smaller alpha scope needs no new Python requirement, runner framework, or
option migration.

## Before and after

| Area | Before | After |
| --- | --- | --- |
| What a job runs | One command or a declared sequence of steps | One container command |
| Workflow definition | Step IDs, commands, inputs from earlier outputs, output declarations | Application code owns the sequence |
| Recovery | Host JSON checkpoints, execution retention, same-release resume and abandon | Application owns progress and recovery; a Onebox retry starts the command again |
| Runtime | Shell runner plus an embedded Python workflow runner | Existing shell runner; no Python requirement for scheduled jobs |
| Operator surface | Job/schedule commands plus four `ob execution` commands | Job/schedule commands only |
| Retention | Live release leases plus persistent execution references | Live leases and container mounts; old checkpoint stores block cleanup until explicit cutover |
| Authored contract | `onebox.run/v1alpha1` with optional `execution` | `onebox.run/v1alpha2`; old identity and removed field are rejected |

## Due diligence

Reviewed schema/defaults/validation, generated systemd units and shell scripts,
manual job admission, history/notifier behavior, workflow checkpoints,
deployment and release-retention interactions, CLI registrations, documentation,
and test coverage. The audit covered the source at
`c0d20fc6dbe40cced3eab92064acab2f42c60eeb` and the current sibling manifests.

- [Issue #160](https://github.com/labstack/onebox/issues/160) introduced durable
  executions around Monk's sync/index sequence and Goal's refresh work. Their
  current `ob.yml` files contain no `execution` declaration. Their Porter
  applications use Temporal and own their application workflows. No live host
  was inspected; an unused current declaration does not prove an old host has
  no checkpoint files. A temporary loader audit confirmed that both current
  sibling manifests load after changing only the Application API identity; the
  editor schema URL should also be updated.
- Ordinary jobs are used: Monk declares a weekly phase-none
  job; both repositories declare deployment jobs and migration data effects.
  These need scheduling and deployment safety, not Onebox step/output schemas.
- [Issue #133](https://github.com/labstack/onebox/issues/133) documents a separate
  need for long jobs to coexist with deployments. Keep optional pinned jobs and
  their live release leases; removing them would make deployments wait for the
  entire command.
- Existing tests cover ordinary retries, timeouts, locks, inputs, notifications,
  release leases, manual approval bindings, and outcome records. The retired
  Python protocol had Go-integrated tests. Its separate server workflow test
  was not selected by the normal server CI matrix; it is removed with the
  feature rather than presented as verification of the remaining scheduler.

## What changed

1. Removed the execution field/types, step validator/defaults/schema rules,
   checkpoint package, Python runtime/tests, durable runner generator, and
   feature-specific server test.
2. Removed execution recovery methods, operation kinds/request fields, CLI
   commands, output registrations, and their diagnostic codes. Removed checkpoint
   compatibility invalidation from ordinary jobs and exec.
3. Kept single-command schedules, whole-command retries, bounded timeout and
   shutdown grace, non-secret inputs, notifications, history/logs, timer pause and
   resume, exclusive/pinned deployment coordination, and release lease safety.
   Deployment resume/abort and migration result evidence remain supported.
4. Added a read-only legacy-store guard with explicit cutover guidance. Existing
   host checkpoints are never auto-deleted or converted. Old history rows remain
   readable even when they include extra execution metadata.
5. Advanced only the authored Application identity to v1alpha2, as required by
   the accepted version policy. Updated fixtures, examples, schema publication,
   CLI generation, product scope, capability inventory, and the scheduling guide.
   Historical design records remain available and the retired feature plan is
   marked superseded. A narrow immutable-snapshot reader preserves ordinary
   v1alpha1 release replay, while rejecting removed workflow fields.

## Cutover checklist

For ordinary jobs, change `apiVersion` to `onebox.run/v1alpha2`, update the editor
schema URL, and regenerate outstanding plans/approvals. Current Monk and Goal
manifests need those identity changes; this change does not edit either sibling
repository.

For an execution-enabled deployment, keep the previous binary available. Pause
old timers, finish or abandon saved work, verify that no job is running or
queued, and archive the old store outside Onebox's application directory while
preserving needed releases/data. Move the workflow into the application command,
deploy a workflow-free v1alpha1 release with the previous binary, then switch
to v1alpha2, verify a manual run, and resume timers. Old units do not change
merely because the workstation binary was upgraded.

Ordinary v1alpha1 snapshots remain readable for deployment and recovery without
rewriting their identity or bytes. Snapshots containing `execution` still need
the previous binary; finish their recovery before upgrading and establish a
workflow-free serving release. Do not relabel stored evidence. See the
[schedule guide](../../site/src/content/docs/guides/schedule-a-job.mdx) for details.

## Validation

- `just ci` passes: module/format checks, Go vet and tests, generated-doc/schema
  checks, documentation site build, lint, vulnerability check, and workflow checks.
- `just e2e` passes on the local Colima Docker daemon (311.6 seconds). A further
  Docker deployment regression passes after the snapshot compatibility change
  (128.5 seconds): an ordinary v1alpha1 predecessor upgrades to v1alpha2 while
  its immutable snapshot stays unchanged.
- Regression tests reject the old authored API and removed execution field,
  reject legacy stores before schedule/request writes or release cleanup, preserve
  old journal outcomes, and permit ordinary old release snapshot replay. The
  guard's shell check is exercised against actual absent paths, directories,
  files, and dangling symlinks.
- Both current sibling manifests passed a temporary loader audit after only
  the API identity update. The temporary audit test was removed from the project.
- Embedded and website schemas match; local report links resolve, whitespace
  checks pass, and the updated prose passes the typo check. An independent
  namespace scan covered existing tracked and new files because the existing CI
  recipe emits missing-file warnings for unstaged deletions.

Not run: real-server/systemd E2E, reboot recovery, or live-host cutover. No
production host or sibling repository was changed.
