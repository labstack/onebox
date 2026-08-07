## Purpose

Keeps a Onebox-owned host from silently exhausting storage through container logs, obsolete images, or unmanaged growth while preserving every artifact required for recovery.

## ADDED Requirements

### Requirement: Generated containers have bounded logs

Onebox SHALL apply and verify an effective log-rotation policy for every
generated workload, supporting service, and managed proxy container. A custom
logging driver or Compose reference whose retention cannot be verified SHALL
be reported as externally managed rather than protected. Existing projects
without authored values SHALL receive stable documented defaults, with their
default origin visible in canonical output.

#### Scenario: Default rotation is generated
- **GIVEN** a generated container with no authored logging policy
- **WHEN** its runtime is rendered
- **THEN** the runtime carries the documented size and generation limits and canonical output marks them as defaults

#### Scenario: Rotation cannot be verified
- **GIVEN** a referenced Compose workload selects an unsupported logging driver
- **WHEN** status or doctor evaluates log protection
- **THEN** it reports that workload as externally managed and never claims rotation is active

### Requirement: Image pruning preserves every recovery image

Onebox SHALL identify images referenced by the current release, every retained
release, supporting services, restore staging, scheduled jobs, and the managed
proxy before pruning. It SHALL delete only images whose Onebox ownership and
unreachability from those roots are proven, and SHALL never invoke an
unscoped system-wide prune.

#### Scenario: Retained rollback image
- **GIVEN** an old image is referenced by a retained release
- **WHEN** housekeeping runs
- **THEN** the image is preserved even when no running container uses it

#### Scenario: Foreign image
- **GIVEN** an unused image has no Onebox ownership evidence
- **WHEN** housekeeping runs
- **THEN** the image is left untouched

#### Scenario: Interrupted prune
- **GIVEN** image pruning was cancelled or the runner disconnected
- **WHEN** housekeeping is retried
- **THEN** it recomputes reachability from current state before any further deletion

### Requirement: Disk pressure is observable and gates unsafe growth

Onebox SHALL report absolute and percentage disk headroom for its base path,
container storage, backup staging, and restore staging where they differ.
Warning and critical thresholds SHALL be explicit effective values. A critical
state SHALL block deployments, backups, and restore staging that increase disk
usage while preserving read, cleanup-plan, backup-list, and recovery commands.
The host contract SHALL permit a distinct restore-drill staging filesystem.
Before a drill, the selected driver SHALL publish a bounded second-copy
footprint and Onebox SHALL compare it with effective headroom.

#### Scenario: Critical disk pressure
- **GIVEN** a relevant filesystem is below its critical threshold
- **WHEN** a new deploy or restore is planned
- **THEN** the plan fails with code `disk_pressure_critical` and names safe inspection and cleanup commands

#### Scenario: Read operation under pressure
- **GIVEN** critical disk pressure
- **WHEN** status or audit is requested
- **THEN** the read completes without running cleanup

#### Scenario: Restore drill lacks second-copy headroom
- **GIVEN** the bounded driver footprint exceeds headroom on the effective drill staging filesystem
- **WHEN** the drill timer runs
- **THEN** no restore bytes are materialized and status reports `drill_deferred_capacity`, required bytes, the selected filesystem, and safe cleanup or reconfiguration commands separately from backup integrity

### Requirement: Housekeeping is host-native and inspectable

Onebox SHALL install exact host schedules for log verification, disk checks,
and eligible image pruning. Generated units and their effective commands SHALL
be inspectable with secrets redacted, survive reboot, and be removed when the
capability is disabled. Disabling or destroying the application SHALL NOT
delete volumes, remote backups, or foreign runtime resources.

#### Scenario: Timer fires without Onebox connected
- **GIVEN** housekeeping is enabled and the CLI is absent
- **WHEN** the host schedule becomes due
- **THEN** the bounded housekeeping operation runs and records its result

#### Scenario: Schedule is removed
- **GIVEN** housekeeping was previously enabled
- **WHEN** configuration disables it and an authorized convergence runs
- **THEN** the generated timer is removed while protected data remains intact

### Requirement: Hygiene operations expose structured results

Housekeeping plan, run, and status commands SHALL use versioned structured
output with stable resource identifiers, reclaimed-byte counts, preserved
roots, typed failures, and resolving next commands. Repeating an operation
identity SHALL be idempotent.

#### Scenario: Structured prune result
- **GIVEN** eligible images were pruned
- **WHEN** JSON output is requested
- **THEN** the result identifies deleted Onebox-owned images, reclaimed bytes, and every protected root without including command noise
