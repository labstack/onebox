# Core engine boundary review

## Decision

Onebox should be the **transactional operating engine for one declared
application on one host**, not a general host-management agent or a catalogue
of containers it happens to be able to start.

The core owns a capability only when it can model the desired state, execute a
bounded lifecycle, verify the promised outcome, retain evidence, and run a
tested recovery path. Everything else is either a user-defined workload or a
documented integration. A container being easy to launch is not enough to make
its semantics a Onebox responsibility.

This retains the product's strongest property: a request is not presented as a
guarantee until the engine has established and checked that guarantee. It is
also the scaling boundary. Adding more integrations does not make the core a
larger pile of special cases unless the integration can pass the same ownership
test.

## Existing evidence

The repository already has the correct foundation:

- the lifecycle engine takes a host lock and fence, journals each phase, and
  refuses a stale runner before it can mutate (`internal/engine/deploy.go`);
- plans, approval binding, derived Compose, release retention, and health-gated
  convergence make deployment an evidence-backed transaction rather than a
  remote shell script (`docs/product.md`);
- Postgres earns a backup contract through an executable archive, base-backup,
  verification, restore, and drill path; generic workload volumes are
  explicitly not claimed as backed up (`docs/product.md`);
- durable executions have already exposed the cost of extending the host
  runtime: a Python helper, systemd units, Docker/Compose inspection, host
  files, locks, and crash-state rules (`internal/durable/runner.py`,
  `docs/plans/2026-09-06-durable-job-executions.md`).

The current product statement is therefore a better guide than a feature list:
Onebox owns explicit operational state and resources, while Docker itself,
Linux, and arbitrary application behavior remain user-owned.

## Independent-review finding: agentless is not stateless

The correct contract is **no resident Onebox management/control daemon and no
network control port**, not “nothing is installed or persists on the host.”
The shipped system already writes release state, journals, systemd units and
timers, generated runners/notifiers, a durable-execution helper, backup
credentials/wrappers, and run records. Those are legitimate application
operations state; denying their existence creates an artificial constraint on
safe implementation.

Keep SSH as the control channel and systemd as the supervisor for unattended
work. Replace host-language and generated-script complexity with a finite,
versioned on-demand helper when doing so materially improves correctness. Do
not introduce a persistent management daemon unless a concrete always-on
requirement cannot be met by systemd plus a short-lived helper. A daemon would
add an upgrade protocol, durable schema migration, local authorization,
availability monitoring, and an incident-response obligation without improving
the single-host availability boundary.

## Ownership test

A proposed capability is **Managed** only if every answer below is yes.

| Test | Required evidence |
| --- | --- |
| Intent | A small declarative model describes what must happen without embedding arbitrary shell or vendor configuration. |
| Preconditions | The engine can identify supported hosts, storage, versions, and exclusive ownership before mutation. |
| Execution | The action is bounded, idempotent or safely resumable, fenced from stale actors, and has clear concurrency rules. |
| Verification | A machine-checkable observation proves the promised result, not merely that a command exited zero. |
| Recovery | Restore/rollback is specified, exercised in CI, and does not overwrite the surviving state before the replacement is proven. |
| Lifecycle | Upgrade, credential rotation, retention, removal, and failure reporting are defined. |
| Support matrix | The supported engine, filesystem, container/runtime version, and security model are finite and tested. |

If any test is no, the capability is **Run** or **External**. Onebox may still
wire its image, mounts, secrets, resource limits, logs, and health check, but
must say that backup/recovery semantics belong to the user and tool vendor.

## What the core should own

| Area | Core responsibility | Boundary |
| --- | --- | --- |
| Intent and policy | Parse, validate, canonicalize, plan, approve, and classify risk. | Do not become a general IaC or scripting language. |
| Transaction execution | Locks, fences, journals, resumability, cancellation, and auditable results. | Do not promise rollback for unmodelled external effects. |
| Application runtime | Generate and reconcile the declared Compose runtime, names, networks, release directories, health gates, and release retention. | Do not install or upgrade Docker, Linux, or host networking. |
| Secrets | Resolve declared secrets, stage them privately, rotate them through a defined flow, and avoid logging them. | Do not become a general secret manager or expose unscoped host credentials. |
| Selected service drivers | Own a small set of database/service drivers with a pinned image, migration path, health semantics, backup, restore, and drill. | Unknown services remain Run/External. |
| Scheduled work | Schedule declared jobs, prevent overlap, retain evidence, and coordinate jobs with deployment. | Job business effects and idempotency remain application-owned. |
| Observation | Report the observed state, drift relevant to owned resources, health, backup evidence, and actionable refusal reasons. | Do not imply full host monitoring, SIEM, or universal log management. |

## What should remain integration-first

| Need | Onebox role | User/tool role |
| --- | --- | --- |
| Generic workload-volume backup | Initially document a reference deployment and mark it user-owned. Promote only per storage/consistency driver. | Choose the tool, RPO, consistency method, retention, and recovery procedure. |
| Continuous file replication | Run the declared container and expose its health/logs. | Own watcher behavior, versioning, ransomware protection, and restore semantics. |
| Host storage snapshots | Detect a declared supported backend only after a driver exists. | ZFS/Btrfs/LVM layout and replication remain external until tested end-to-end. |
| Monitoring, log shipping, malware scanning | Provide ordinary workload wiring. | Own alert routes, data retention, query semantics, and incident response. |
| Arbitrary installers and host tuning | Execute an explicit bootstrap hook under the normal journal/lock boundary. | Own package lifecycle, distro matrix, CVE response, and rollback. |

This is not a refusal to support those outcomes. It is a rule against silently
turning an image pull into a support commitment.

## Backup implications

Restic is suitable for efficient scheduled file snapshots: it is a command-line
program, not a daemon, and delegates scheduling to systemd/cron. [Restic's
documentation](https://restic.readthedocs.io/en/stable/040_backup.html) also
notes that recurring invocations need overlap control. It is therefore a good
engine for a bounded snapshot driver, but not evidence of continuous protection
or application consistency.

Storage-native drivers can offer a stronger contract. For example, zrepl takes
periodic ZFS snapshots and incrementally replicates them, with hooks for
quiescing selected applications. Its own documentation frames the interval as
the RPO and requires replication health to be monitored. [zrepl snapshotting](https://zrepl.github.io/configuration/snapshotting.html)
and [continuous-server example](https://zrepl.github.io/quickstart/continuous_server_backup.html)
show both the promise and the prerequisite: this is a ZFS product feature, not
a generic bind-mount feature.

The appropriate progression is:

1. Document user-defined backup/replication workloads now, clearly outside the
   Onebox backup guarantee.
2. Add a **snapshot-volume** driver only with explicit consistency modes
   (`quiesced`, then app-aware hooks), restore into a fresh destination, and a
   CI restore drill.
3. Add a **ZFS replication** driver only on a finite ZFS host profile.
4. Keep database log shipping in the database driver; it has different recovery
   semantics from a filesystem copy.

## Host runtime and delivery

Use OCI images as the standard distribution format for Onebox-owned executable
components, with immutable multi-architecture digests. That is a delivery
choice, not an ownership claim. The image should contain a narrow, versioned
runtime API and be invoked only for operations the corresponding driver owns.

`onebox-kit` is appropriate for ephemeral helpers. If a resident controller is
actually needed later, name it `onebox-agent` and give it a separate lifecycle,
database migration, health, upgrade, and incident-response contract. Do not
quietly turn a helper image into a daemon.

An agent that controls Docker is privileged: Docker documents that a user with
daemon access can create containers that alter arbitrary host paths. [Docker
Engine security](https://docs.docker.com/engine/security/) Thus an agent or
worker receiving the Docker socket is a core trusted-computing-base component,
not a normal workload. Prefer generated static mounts for backup workers; grant
the socket only where the operation demonstrably needs host runtime control.

## Recommendation

Keep the current transactional engine as the product core and establish the
ownership test as the admission rule for every new service, backup, or host
tool. Do not make generic continuous backup a core promise now. Deliver a
documented user-defined container pattern first; add managed drivers one
storage/application class at a time as their verification and restore evidence
exist.

Revisit a resident `onebox-agent` only when the roadmap contains at least two
funded, always-on capabilities that cannot be expressed as a systemd-timed
operation plus an ephemeral worker. At that point the agent is not delivery
plumbing: it is a deliberate replacement for part of the engine's execution
model and must be designed and operated as such.

## Sources

1. Onebox, [Product direction](../product.md) and repository lifecycle sources
   cited above, accessed 2026-09-13.
2. Restic, [Backing up](https://restic.readthedocs.io/en/stable/040_backup.html),
   accessed 2026-09-13.
3. zrepl, [Taking snapshots](https://zrepl.github.io/configuration/snapshotting.html)
   and [Continuous backup of a server](https://zrepl.github.io/quickstart/continuous_server_backup.html),
   accessed 2026-09-13.
4. Docker, [Engine security](https://docs.docker.com/engine/security/), accessed
   2026-09-13.
