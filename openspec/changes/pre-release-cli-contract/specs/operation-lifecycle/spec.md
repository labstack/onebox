## Purpose

Defines the observable release-state and recovery guarantees that let operators and coding agents trust deploy, rollback, abort, and secret-rotation outcomes.

## ADDED Requirements

### Requirement: Releases follow an explicit state machine

Each recognized release SHALL carry a versioned manifest containing its identifier, kind (`application` or `bootstrap`), state (`staged`, `verified`, `serving`, `superseded`, `failed`, or `aborted`), terminal operation outcome, and predecessor identifier. State transitions SHALL be atomic and monotonic except that rollback is a new activation transition. A current symlink and manifest disagreement SHALL be treated as typed incomplete activation and reconciled from the durable checkpoint, never guessed from directory ordering.

A rollback target SHALL be the current serving application's recorded predecessor whose manifest proves a prior healthy activation. Bootstrap, upload, staged, failed-before-activation, aborted, corrupt, and unknown-manifest directories SHALL NOT be rollback candidates. If an operation fails after healthy activation, its manifest SHALL record the failed operation outcome separately while preserving the truthful `serving` state.

#### Scenario: Bootstrap follows a serving release
- **WHEN** a bootstrap snapshot is newer than the previous serving application release
- **THEN** rollback selects the previous serving application release and never the bootstrap snapshot

#### Scenario: Operation fails after healthy activation
- **WHEN** a post-activation action fails after the release became healthy and current
- **THEN** the manifest records a failed operation outcome while retaining the truthful serving state and predecessor link

#### Scenario: Repeated rollback
- **WHEN** release B rolls back to predecessor A and rollback is requested again
- **THEN** A's new activation transition records B as its predecessor so the next rollback deterministically targets B

#### Scenario: Crash between symlink and manifest transition
- **WHEN** the runner crashes with the current symlink and serving state out of agreement
- **THEN** status reports typed incomplete activation and resume or abort reconciles it from the checkpoint

#### Scenario: Current release has no manifest
- **WHEN** the current release lacks a valid serving application manifest
- **THEN** mutation fails before effects and the invalid development state must be recreated; no compatibility state is inferred from older journals

#### Scenario: Manifest is corrupt or unknown
- **WHEN** a release manifest cannot be validated
- **THEN** it is excluded from rollback without deletion and status reports the specific evidence failure

#### Scenario: No previous serving release exists
- **WHEN** rollback is requested and no eligible prior serving release exists
- **THEN** Onebox makes no mutation and returns typed error `rollback_target_missing` with `ob deploy` as the resolving command

### Requirement: Retention is distinct from rollback eligibility

Retention SHALL preserve the current serving release and its configured eligible predecessor chain. It SHALL separately garbage-collect expired bootstrap snapshots, uploads, failed stages, aborted releases, and unknown entries according to explicit age and evidence rules. A directory's ineligibility for rollback SHALL NOT make it immortal and SHALL NOT make it immediately deletable.

Retention SHALL fail closed when any manifest needed to establish the protected predecessor chain is missing, unreadable, or invalid. It SHALL select no deletion candidates from incomplete evidence rather than treating the unreadable predecessor as the end of the chain.

Refusing to select candidates SHALL NOT fail the operation that requested cleanup. The refusal SHALL be reported to the operator, surfaced in the operation's progress events, and journaled with a value-free note; an operation whose release is already serving SHALL still reach its terminal state. A run that refuses to select release candidates SHALL delete nothing at all, including operation journals, because those journals are themselves the evidence that protects release directories whose manifests cannot be read.

#### Scenario: Failed stage exceeds retention age
- **WHEN** a failed staged application release is older than the configured garbage-collection threshold and no incomplete operation references it
- **THEN** retention removes it without counting it as a rollback release

#### Scenario: Incomplete operation references a release
- **WHEN** any checkpoint references a staged, failed, or unknown release directory
- **THEN** retention preserves that directory until recovery finalizes the checkpoint

#### Scenario: Chain evidence is unusable during a deploy
- **WHEN** retention cannot establish the protected predecessor chain while completing a deploy whose release is already serving
- **THEN** no release directory and no journal is deleted, the refusal is reported and journaled as a skipped step, and the deploy still records a successful terminal state

### Requirement: Recovery success is a complete state transition

For an operation whose durable effect gate is rollback-safe, abort or automatic rollback SHALL report success only after it removes every newcomer owned by the interrupted operation, restores the previous serving release, verifies the restored workloads, finalizes the interrupted operation, and removes its incomplete marker. A retry after disconnection or process crash SHALL converge idempotently on the same state.

If migration or unknown-effect evidence closes the rollback gate, automatic rollback SHALL NOT run. Abort SHALL fail before restoring or removing containers unless the existing explicit break-glass authorization is present; it SHALL retain the checkpoint and return the specific resolving actions.

Only the newest deploy SHALL be recoverable. A deploy superseded by a later deploy that completed, aborted, or was rolled back SHALL NOT be reported as incomplete nor offered to resume or abort: the later operation re-released every workload and settled the host's release state, so completing the older one would re-activate a release the host has already moved past and aborting it would restore a staler predecessor still. Its records SHALL remain available as audit history.

An operation interrupted after activation SHALL NOT be recovered by replaying its choreography or by reverting a healthy release. Resume SHALL complete only the post-activation steps that remain, and SHALL do so only when the recorded activation, the current release, the serving manifest and its recorded predecessor, the absence of an open activation checkpoint, and the live workload release labels all agree; on any disagreement it SHALL run no post-activation step, change no release, symlink, or workload state, and return typed error `finalize_refused`. Each post-activation step SHALL be journaled individually so a repeated finalize never re-runs a step that already succeeded.

#### Scenario: Rolling newcomer exists only in created state
- **WHEN** abort runs after a rolling replacement was created but could not start
- **THEN** the newcomer is removed before abort reports success

#### Scenario: Automatic rollback succeeds
- **WHEN** a reversible deploy fails verification and the prior release is restored healthy
- **THEN** status reports the prior release complete and non-divergent with no incomplete deployment

#### Scenario: Recovery cannot complete
- **WHEN** any recovery postcondition cannot be established
- **THEN** Onebox retains honest incomplete state and returns a typed recovery error rather than reporting success

#### Scenario: Agent retries recovery after disconnect
- **WHEN** the client disconnects during abort and retries the same command
- **THEN** recovery resumes or returns the already-established result without creating additional containers or state transitions

#### Scenario: Migration effect closes the gate
- **WHEN** a failed deploy contains an uncovered migration or unknown data effect
- **THEN** automatic rollback is refused and abort returns `migration_gate_closed` with `ob resume` and the authorized break-glass abort as resolving actions

#### Scenario: Break-glass abort is authorized
- **WHEN** the operator supplies the existing explicit migration-gate override
- **THEN** abort records that authority and performs the same complete recovery postconditions without claiming application-data reversal

#### Scenario: A post-activation step fails
- **WHEN** retention, schedule sync, or the post-deploy hook fails after the release became current and healthy
- **THEN** the release stays serving with a failed operation outcome, and `ob resume` completes the remaining steps and records a successful terminal state

#### Scenario: Finalize evidence disagrees
- **WHEN** the current release, serving manifest, recorded predecessor, activation checkpoint, or live workload labels do not agree that this operation activated the release now serving
- **THEN** no post-activation step runs, no release, symlink, or workload state changes, and the operation returns typed error `finalize_refused`

#### Scenario: An interrupted deploy is superseded
- **WHEN** a deploy is interrupted and a later deploy completes, aborts, or is rolled back
- **THEN** status reports no incomplete deployment, resume and abort decline the superseded operation, and its journal remains in the audit history

#### Scenario: A non-idempotent post-activation step already succeeded
- **WHEN** finalize runs again after the post-deploy hook already recorded a successful result
- **THEN** the hook does not run a second time and only the steps that remain are executed

### Requirement: Secret rotation proves live adoption

`secrets push` SHALL require exact equality between the local and current-release encrypted-entry declaration graph, including path, provider, order, scope, and affected workloads. It SHALL transition between opaque secret generations through a durable checkpoint. Generated files and containers SHALL identify only the opaque generation identifier; Onebox SHALL NOT expose raw or unkeyed secret-content hashes.

Once the declaration graph is proven equal, rotation SHALL derive workload topology and operational attributes from the current release snapshot rather than mutable working-tree configuration. Startup SHALL remove abandoned plaintext upload directories and any candidate generation directory that is named by neither the committed runtime nor a durable checkpoint. A checkpoint-publication failure SHALL remove the installed candidate before returning. Successful commit SHALL remove superseded generations before clearing the checkpoint; a cleanup failure SHALL retain typed retryable recovery state instead of reporting success.

The terminal invariant SHALL be exactly one of: every affected staged file and live workload uses the old generation; every affected staged file and live workload uses the new generation; or the operation remains typed-incomplete with its checkpoint. Success SHALL require the all-new state, while an unchanged input SHALL return a successful no-op. Plaintext values SHALL NOT appear in plans, arguments, output, logs, journals, or evidence added by Onebox.

#### Scenario: Secret content changes
- **WHEN** an encrypted entry's content hash differs from the current serving release
- **THEN** a new opaque generation is prepared, affected workloads are recreated, and success is reported only after every live container and staged file identifies that generation

#### Scenario: Secret content is unchanged
- **WHEN** every encrypted entry matches the current serving release
- **THEN** the command returns a successful no-op without restarting a workload

#### Scenario: Declaration graph differs
- **WHEN** local configuration adds, removes, reorders, moves, or changes the provider of an encrypted entry relative to the current release
- **THEN** the command makes no mutation and returns typed error `secret_declaration_not_deployed` with `ob deploy` as the resolving command

#### Scenario: Rotation fails after partial replacement
- **WHEN** one affected workload adopts the new generation and a later replacement fails
- **THEN** recovery converges every affected workload and staged file to the old generation or retains typed-incomplete state; it never reports success for a mixed generation

#### Scenario: Runner crashes during generation transition
- **WHEN** the runner disconnects or crashes during prepare, replacement, verification, or commit
- **THEN** retry resumes from the durable generation checkpoint without inventing a terminal state

#### Scenario: Runner crashes before checkpoint publication
- **WHEN** a plaintext candidate generation was installed but its checkpoint was not published
- **THEN** the next rotation preserves the committed generation, removes the uncheckpointed candidate before comparison or replacement, and never treats the orphan as resumable state

#### Scenario: Working tree changes after the current release
- **WHEN** workload replica or operational settings differ locally while the encrypted declaration graph is unchanged
- **THEN** `secrets push` rotates the deployed snapshot's workloads and settings and does not apply undeployed working-tree behavior

#### Scenario: Cleanup fails after commit
- **WHEN** live workloads and staged files use the new generation but removal of the superseded generation fails
- **THEN** the checkpoint remains and retry completes cleanup before a terminal success is returned

### Requirement: Manual jobs use the same sealed operation model

Onebox SHALL expose `ob job plan <id>` and `ob job run` as the canonical one-shot job operation. The plan SHALL bind the application, environment, server, current serving release and runtime digest, job identifier, immutable image, declared data effect, and expiry. `ob approve` SHALL confirm that exact plan and any required backup-report digest; `ob job run --plan --approval --backup-report` SHALL refuse stale release state, changed report facts, or mismatched confirmation before execution.

A human MAY use `ob job run <id>` to plan and confirm interactively through the same local confirmation boundary. Migration and unknown-effect jobs SHALL retain strong confirmation, backup-report, result-evidence, and rollback-gate requirements. Scheduled execution SHALL use the existing signed schedule envelope and invoke the same canonical engine operation without interactive confirmation.

#### Scenario: Agent invokes an unscheduled manual job
- **WHEN** an agent creates a job plan, records a separate plan-bound local confirmation, and applies both while the bound release is current
- **THEN** the job runs once, journals a versioned result, and returns a compact structured terminal outcome

#### Scenario: Current release changes after job planning
- **WHEN** a deployment changes the serving release before the job plan is applied
- **THEN** job execution is refused before container creation and directs the agent to re-plan

#### Scenario: Manual migration job lacks evidence
- **WHEN** a manual migration job requires a backup report and none is supplied
- **THEN** execution fails closed before running the job and names the report flag and planning output
