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

## Contract

The Application contract is `onebox.run/v1alpha2`. Project declarations and
immutable release snapshots use the same strict loader and schema.

Machine artifact schema identities remain independent. Deployment recovery,
migration evidence, backups, and approvals keep their wire formats. Job run
records continue to use the host journal as their outcome authority.

## Scope kept small

This change removes behavior. It does not reorganize all schedule options,
introduce another runner abstraction, require Python for ordinary jobs, or
replace the manual request protocol. Those would expand the alpha change
without establishing a user need. Pinned jobs remain because deployment should
not have to wait hours for a read-only command; the existing live lease protects
the immutable release while it runs.
