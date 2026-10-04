# Jobs run commands; applications own workflows

- Status: accepted and implemented
- Date: 2026-10-04
- Scope: authored Application `onebox.run/v1alpha2`, job runtime, CLI, documentation
- Supersedes: [durable job executions](../plans/2026-09-06-durable-job-executions.md)

## Decision

A Onebox job runs one container command. A schedule adds a host systemd timer,
bounded retries, timeout, deployment coordination, and run records. Applications
own multi-step workflows, checkpoint recovery, output handoff, and effect
deduplication. Onebox does not provide a second workflow engine.

Remove `workloads.<name>.execution`, its retention and step definitions, the
Python checkpoint runner, and `ob execution list/inspect/resume/abandon`.
Remove execution-specific compatibility invalidation and checkpoint-based
release pinning. Keep ordinary job inputs, notifications, history/logs,
pause/resume of timers, sealed job approvals, migration result evidence,
deployment recovery, and optional pinned scheduled jobs with live release leases.

The workflow feature was added for the Monk sync/index sequence and Goal refresh
use cases in [#160](https://github.com/labstack/onebox/issues/160). Their current
manifests declare no `execution` block, and their Porter applications own their
workflow machinery. These examples demonstrate application needs; they do not
justify a general workflow contract in Onebox. This audit examined the current
checkouts, not their deployed host state. A temporary loader audit confirmed
that both manifests load with only the Application API identity updated.

## Contract and cutover

Follow the [Application version policy](2026-09-20-application-v1alpha1.md):
this incompatible removal advances the authored identity to
`onebox.run/v1alpha2`. Authored manifests have one accepted identity and no
field aliases or automatic conversion. Ordinary manifests update `apiVersion`; execution-enabled jobs move
their workflow into their command before removing the block. Plans and approvals
bound to changed manifest bytes must be regenerated.

Machine artifact schema identities remain independent. Existing deployment
recovery state, migration evidence, backups, and approvals retain their wire
formats. Removing the optional execution metadata from new job-run responses
does not change the run/outcome authority in the host journal; old journal rows
with extra execution metadata can still be read.

A small read-only guard refuses deployment preflight, schedule installation,
job starts, container exec, and release retention if the retired host
`schedule/executions` path exists, including an empty store or dangling symlink.
It refuses inspection failures too. It neither parses checkpoints nor runs
Python, rewrites state, or deletes releases. This prevents an upgrade from
silently replacing old runners or losing their release references.

Use the previous binary to pause old execution-enabled timers and finish or
abandon executions. Verify no work is running or queued, preserve needed data
and releases, then archive the store outside the application directory. Pause
old timers even when the store is absent: an unused timer may not have created
it yet. Reconcile the new units after deploying the application command and
resume timers only after verifying a run.

Deployment must read its predecessor snapshot before retiring removed workloads.
To keep ordinary existing deployments upgradeable, the immutable snapshot reader
also accepts v1alpha1 identity when its fields satisfy the current model. It
preserves the original identity and bytes; it never rewrites host artifacts or
allows old authored manifests through the public loader. Unknown fields,
including `execution`, remain errors. This is a bounded recovery compatibility
decision, separate from the authored contract.

For execution-enabled installations, use the previous binary to deploy a
workflow-free v1alpha1 release after decommissioning old executions and before
switching to v1alpha2. Old workflow snapshots still need the previous binary for
rollback or recovery. The public
[schedule guide](../../site/src/content/docs/guides/schedule-a-job.mdx) contains
the operator cutover steps.

## Scope kept small

This change removes behavior. It does not reorganize all schedule options,
introduce another runner abstraction, require Python for ordinary jobs, or
replace the manual request protocol. Those would expand the alpha change
without establishing a user need. Pinned jobs remain because deployment should
not have to wait hours for a read-only command; the existing live lease protects
the immutable release while it runs.
