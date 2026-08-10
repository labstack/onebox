## Why

Onebox already has the right safety model, but black-box testing exposed places where the CLI reports a stronger outcome than the engine actually established. Before the first release, the product should tighten those contracts instead of carrying compatibility for ambiguous release state, partial cleanup, mutable execution inputs, or adapter inconsistencies.

## What Changes

- **BREAKING** Reject rolling workloads that bind fixed host ports, because a replacement replica cannot coexist with the serving replica.
- **BREAKING** Treat `when: manual` jobs as manual-only: they may be scheduled or invoked through a sealed `ob job` operation, but never inserted into a deploy operation.
- Pin every image in the bound application release runtime, including jobs and Compose-referenced services, and bind the resolved references into the rendered runtime.
- Give releases an explicit lifecycle state machine and predecessor link so rollback selects the release that previously served, never a bootstrap snapshot, failed stage, or upload directory.
- Make abort and automatic rollback transactional postconditions: remove newcomers, restore the prior serving release, finalize the interrupted journal, and leave `status` non-divergent.
- Make secret rotation an opaque, checkpointed generation transition that ends entirely on the old generation, entirely on the new generation, or honestly incomplete.
- Make the CLI adapter consistent: every command honors `--config`; Onebox-run supporting services are valid `logs` and `exec` targets; no-change secret editing succeeds; and a small command/output matrix defines JSON, NDJSON, passthrough, cancellation, and typed failures.
- **BREAKING** Replace the public `backup-evidence template|create` ceremony and `--backup-evidence` input with a plan-produced backup report and `--backup-report`; execution validates the report and seals the internal receipt at the consequential boundary.
- **BREAKING** Give every safety override one exact name and effect: lock breaking, mount detachment, proxy conflict, and migration-gate override SHALL NOT share a generic force boolean.
- State the approval boundary truthfully: `ob approve` creates a plan-bound local human confirmation, not an authenticated identity-provider grant, and structured output SHALL expose that source without implying otherwise.
- Keep one application identity per target host for the first release; the managed proxy is host-scoped but SHALL NOT implement a cross-application registry or conflict override.
- Make every declared encrypted entry addressable through `ob secrets list` and `ob secrets edit <entry-id>`, distinguish diagnostic/next commands from commands that actually resolve an error, and require arbitrary `exec` use to carry an auditable reason.
- Keep the interface meaningful: retain distinct validation, rendering, runner, target, planning, recovery, and inspection commands while removing only adapter ceremonies that create no independent authority or effect.

## Capabilities

### New Capabilities

- `operation-lifecycle`: Defines release eligibility, abort and rollback finalization, and secret-rotation postconditions.
- `cli-contract`: Defines uniform configuration resolution, resource targeting, no-op behavior, and structured command results.

### Modified Capabilities

- `project-schema`: Adds semantic validation for rolling host-port conflicts and makes manual job execution semantics explicit.
- `runtime-generation`: Extends sealed-plan image pinning and rendered-runtime substitution to jobs.

## Impact

The changes affect project validation, plan/render generation, remote release metadata, rollback/abort cleanup, secret rotation, status reconciliation, approval wording, backup-report artifacts, proxy ownership, exact override fields, and the Cobra CLI adapters for jobs, eject, logs, exec, secrets, approval, and deploy. Existing valid applications continue to work except configurations that relied on an impossible rolling fixed-port combination, on `manual` jobs running during deploy, on cross-application proxy registration, or on the unreleased backup-evidence and generic-force spellings.

No service changes tier: PostgreSQL and Redis remain Onebox-run, Run-tier supporting services, and the change only exposes their existing operational log/exec surface. A backup report is an operator/tool report about an externally created backup, not a backup provider or proof of artifact existence. This change does not create backups, add an authenticated approval provider, perform application data migrations, graduate a service to Managed, alter service upgrade policy, implement restore drills, issue TLS certificates, or add provider-specific behavior. Work is gated in four phases—execution truth, recovery truth, adapter consistency, then pre-release contract correction—and each phase requires focused fault tests before the next begins. Graduation evidence includes the deliberately re-frozen compatibility corpus, strict OpenSpec validation, and the full existing test suite; destructive and data-changing behavior remains fail-closed.
