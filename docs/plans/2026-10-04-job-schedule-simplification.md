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
| Retention | Live release leases plus persistent execution references | Live leases and container mounts |
| Application model | Optional workflow definition | `onebox.run/v1alpha2` with one command per job |

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
  was inspected. A temporary loader audit confirmed that both current
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
4. Kept one strict Application model for project declarations and immutable
   release snapshots.
5. Updated the Application schema, fixtures, examples, CLI generation, product
   scope, capability inventory, and scheduling guide for v1alpha2.

## Using the simpler model

Declare an Application with `apiVersion: onebox.run/v1alpha2`. Give each job one
container command, its data effect, and a schedule when it should run on a timer.
Set retries and a timeout where useful. Keep progress, ordering, and recovery in
the application. Inspect runs with `ob job history` and `ob job logs`.

See the [schedule guide](../../site/src/content/docs/guides/schedule-a-job.mdx)
for complete examples. Sibling repositories were not modified.

## Validation

- `just check`, lint, vulnerability, and environment-namespace checks pass:
  module/format checks, Go vet and tests, generated-doc/schema checks, and the
  documentation site build. Local workflow lint did not finish; workflow parsing
  passes with its ShellCheck integration disabled.
- Docker E2E covers rolling deployment, unchanged immutable snapshots,
  payload digests, and HTTP continuity under the same Application contract.
- Regression tests enforce the single Application identity, reject workflow
  fields before deployment staging, and retain ordinary scheduling controls.
- Both current sibling manifests passed a temporary loader audit after only
  the API identity update. The temporary audit test was removed from the project.
- All 16 rendered landing-page manifest combinations validate. The proxy DNS
  challenge example uses the schema's `dnsChallenge` field.
- Embedded and website schemas match; local report links resolve, whitespace
  checks pass, and the updated prose passes the typo check.

Not run: real-server/systemd E2E, reboot recovery, or live-host testing. No
production host or sibling repository was changed.
