## Context

See [proposal.md](proposal.md) for motivation. The failures span the authoring validator, plan generator, remote release store, engine recovery, secret maintenance, and Cobra adapters, so the fix must preserve one behavior authority rather than patching command output around engine outcomes.

The current release store mixes deploy releases and bootstrap snapshots under one timestamp-ordered directory. Deployment checkpoints survive verification failure by design, but the successful automatic-rollback path does not finalize them. Secret rotation replaces a staged env file in place and calls the normal release choreography; a rolling role can then identify its already-running container as belonging to the same release and skip the replacement that would adopt the new environment.

## Goals / Non-Goals

**Goals:**

- Make impossible projects fail locally before target contact.
- Make sealed plans fully immutable across every runnable workload.
- Establish truthful, retry-safe postconditions for rollback, abort, and secret rotation.
- Make one CLI contract usable without command-specific exceptions.
- Add the minimum persisted metadata necessary to distinguish release kinds.

**Non-Goals:**

- No new adapter, daemon, API service, authenticated approval provider, or generic task runner; `ob job plan|run` and `ob secrets list` are the only new executable command leaves.
- No compatibility shim for unreleased invalid behavior.
- No native backup, restore, data-migration, service-tier, TLS, registry, or provider work. Backup reports describe externally created artifacts and do not graduate a service.
- No attempt to make secret rotation a general configuration deployment; declarations still ship through `deploy`.

## Decisions

### 1. Enforce cross-field constraints during project resolution

The semantic validator will reject `strategy: rolling` when `published_ports` is non-empty. The check belongs beside existing closed-schema and role-specific validation so `validate`, `canonical`, `preview`, and `plan` agree and no SSH connection is opened.

Manual jobs remain in the rendered release because timers and explicit invocation need them, but `DeploymentGraph` will include only jobs whose `when` participates in a release phase. `ob job plan <id>` builds a current-release-bound operation and `ob job run` applies it through the existing approval, migration-evidence, and journal authority. Scheduled timers call the same service operation through their existing signed envelope. This separates runtime presence from automatic deploy execution without inventing a second job model.

Alternative considered: silently downgrade rolling to recreate. Rejected because it changes availability semantics the author reviewed.

### 2. Pin from the rendered execution set, not release order

Image resolution will enumerate every container image in the bound application release runtime in deterministic identifier order. The resulting map remains keyed by workload identifier and is supplied to runtime generation for applications, workers, daemons, jobs, and adopted Compose services. A Compose-authored image tag is replaced in the generated copy while every other authored key remains untouched. Compose build sources require the same explicit resolved-image input as native build workloads and fail planning when it is absent.

Alternative considered: reject tagged job images at authoring time. Rejected because authoring by tag with resolution at plan time is already the ergonomic workload contract.

### 3. Persist a release state machine with predecessor links

Each release directory will carry a mode-0600, versioned manifest with identifier, kind, lifecycle state, terminal operation outcome, predecessor identifier, and transition timestamps. Application transitions are `staged → verified → serving → superseded`; pre-activation failure transitions to `failed` or `aborted`. A post-activation action failure changes the operation outcome but not the truthful serving state.

Activation remains a checkpointed transaction: mark verified, switch `current`, mark serving with its predecessor, then mark the predecessor superseded. A crash at any boundary leaves a state/symlink disagreement that status reports and resume/abort reconciles. Rollback follows the current serving manifest's predecessor and performs a new activation transition whose predecessor is the release being left, making repeated rollback deterministic.

There is no pre-release compatibility path for manifest-less releases. A current release without a valid serving application manifest blocks mutation before effects. Corrupt and unknown manifests are preserved, excluded from rollback, and reported.

Retention is a separate classifier: it preserves current and predecessor-chain releases, protects anything referenced by a checkpoint, and garbage-collects old bootstrap, upload, failed, and aborted entries under explicit age rules.

Alternative considered: infer eligibility from directory identifiers. Rejected because bootstrap, failure, activation order, and repeated rollback are not encoded by names.

### 4. Centralize recovery finalization

Abort and automatic rollback will use one recovery primitive that:

1. removes every container labelled with the interrupted deployment identifier;
2. restores the previous release using its own runtime and project snapshot;
3. verifies it;
4. clears the checkpoint only after verification;
5. journals the final result.

Cleanup targets application ownership plus the exact interrupted release label, never a broad Compose project deletion. Retries repeat the same idempotent steps. A failed postcondition retains the checkpoint and returns a typed error. The primitive runs only while the durable rollback-effect gate is open; a closed migration gate returns before container mutation unless the existing explicit break-glass authority is present, and never claims to reverse application data.

Alternative considered: have `status` ignore a checkpoint after automatic rollback. Rejected because hiding persisted incomplete state would make observation less honest rather than finishing the transition.

### 5. Rotate opaque secret generations transactionally

The service first compares the complete encrypted-entry declaration graph from local configuration with the current release snapshot: path, provider, order, scope, and affected workloads must match exactly. Any difference fails before upload and directs the operator to deploy.

Changed decrypted files are prepared under a new opaque, random generation identifier while the old generation remains intact. A mode-protected checkpoint records `prepared`, each workload replacement, verification, and `committed`. A generated Compose overlay selects the generation and applies an `ob.secret-generation` label; no raw secret hash is emitted. Force replacement proves adoption by changed container identity plus the opaque label.

On failure the recovery primitive recreates every affected role on the old generation. If that cannot complete, the checkpoint remains typed-incomplete; success is impossible while staged files or containers disagree. Retry resumes from the checkpoint after crashes in prepare, replacement, verification, or commit.

This deliberately prefers a guaranteed replacement over reusing release-aware rolling logic that treats the current release as already converged. A later optimization may provide zero-downtime generation replacement without changing the terminal invariants.

### 6. Keep CLI behavior thin and table-driven

Commands continue to delegate lifecycle behavior to the canonical service. Shared helpers own:

- global project-path propagation;
- the finite JSON envelope and streaming NDJSON terminal record;
- typed error conversion and semantically classified command guidance;
- resource target resolution for workloads and managed services;
- no-op, cancellation, and stable exit-status normalization;
- a closed command/output classification table.

Because this is the first public contract, command names and flags are canonical:
there are no deprecated commands, aliases, hidden replacements, or alternate
structured-output spellings. An invalid development invocation fails with the
current command or flag rather than being translated by compatibility code.

The finite envelope is `{schema_version, command, outcome, data|error}` with outcomes `success|no_op|cancelled|error`. NDJSON adds monotonic sequence and exactly one terminal record. Unbounded follow mode rejects JSON. Exec/log output is explicitly operator-controlled passthrough and not promised redaction; Onebox-generated arguments and metadata still never add credentials. Editor commands return a terminal result after the trusted editor exits, while help and completion retain native protocols.

`eject` receives the loaded global path rather than reopening the default. Run-tier supporting services resolve to their driver Compose file/container without copying credentials into command arguments or envelopes. SOPS exit status 200 maps to a successful no-op.

Alternative considered: command-specific fixes. Rejected because they recreate the inconsistency the change is intended to remove.

### 7. Generated state stays inspectable and ownership remains explicit

Release manifests, checkpoints, and opaque secret-generation identifiers live under Onebox's existing mode-protected application state. `status` and `audit` expose identifiers, outcomes, and opaque revisions only. Ejection continues to hand back only the generated application runtime; lifecycle manifests remain Onebox-owned operational state and are not copied into the repository.

### 8. Name local confirmation honestly

`ob approve` remains a deliberate, interactive human ceremony that binds every projected plan field and expires with the plan. Its artifact source is `local_cli`; its digest detects accidental or unaudited modification but is not an issuer signature. Public contracts and documentation will call it a local confirmation, never an authenticated or independently unmintable identity grant. Structured output and audit retain the source so a future authenticated provider can be distinguished without changing the plan contract.

Alternative considered: add an application-local signing key. Rejected because a key configured or readable by the requesting agent does not create an independent authority boundary. Authenticated issuers require separate trust-root, enrollment, revocation, and recovery design.

### 9. Treat a backup report as execution input, not a public evidence product

A saved deploy or job plan may carry a migration-backup requirement. `plan --backup-report-out <path>` writes a strict, secret-free `onebox.run/backup-report/v1alpha1` template bound to that plan digest. External tooling fills the artifact identifiers, integrity results, timestamps, restore-test result, and required-key usability results. `approve --backup-report` binds the exact report digest into the local confirmation, while `deploy` and `job run` accept the same report, revalidate it against current time and the exact plan, then seal and journal an internal receipt per execution attempt.

No public command converts the report into a second file. The conversion adds neither authority nor an external effect, and performing it early creates a second validation timeline. Optional early validation uses the canonical plan/report validator without minting a receipt. Native `ob backup` commands remain reserved until Onebox actually owns backup creation and recovery.

Alternative considered: retain `backup-evidence create`. Rejected because a locally recomputable wrapper can be mistaken for proof and conflicts with the future custody-bearing backup namespace.

### 10. Give each override one typed authority

The canonical request carries separate booleans for breaking a stale lock, permitting an explicitly described mount detachment, and crossing the migration rollback gate. No adapter maps these into one `Force` field. Unsupported service major-version transitions remain unavailable regardless of any mount or lock flag. The first-release one-application host model removes cross-application proxy-conflict override entirely.

Alternative considered: preserve one internal force bit behind precise flags. Rejected because the bug is semantic, not cosmetic: downstream code cannot know which authority the operator actually granted.

### 11. Address encrypted declarations by stable value-free identity

The resolved secret declaration graph assigns each editable encrypted source a stable identifier derived from scope, order, source path, provider, and output path without secret values. `secrets list` returns these identifiers and safe metadata. `secrets edit <entry-id>` edits exactly one SOPS source; omitting the identifier is allowed only when exactly one editable entry exists. External projected credentials remain non-editable and are identified as such.

### 12. Keep the host ownership model singular

Onebox continues to use a host lock and a host-scoped proxy because those resources outlive an application release, but the first-release target state has exactly one application owner. Bootstrap or preflight refuses a different registered application identity. Proxy convergence reads only that owner; destroy no longer offers cross-application preservation or conflict semantics.

### 13. Separate diagnosis, next action, and resolution

Typed errors may expose `diagnostic_command`, `next_command`, and `resolving_command`. A read-only command that gathers more information is diagnostic. A command that advances a multi-step workflow is next. `resolving_command` is present only when executing it can remedy the reported condition. This prevents agents from looping on `status` while preserving deterministic guidance.

### 14. Audit arbitrary container execution without pretending it is safe

`exec` retains its familiar name and passthrough protocol, but requires a bounded single-line reason. Onebox journals the target, target kind, operator, command digest, start time, outcome, and reason; it never records command bytes or passthrough output. The command remains outside release convergence and cannot claim rollback, redaction, or idempotence.

## Risks / Trade-offs

- [Force-recreating application roles during secret rotation can cause a brief gap] → State it in the operation description now; optimize later without weakening generation-wide adoption proof.
- [Strict manifests invalidate old development state] → This is intentional before release; recreate throwaway state instead of shipping permanent migration machinery.
- [Cleanup could remove a healthy container if labels are wrong] → Match both application ownership and exact interrupted release identifier, then verify the previous release before clearing state.
- [Uniform structured output touches many adapters] → Implement it only after execution and recovery gates pass, and add a closed command-matrix golden test.
- [Pinning more images adds registry calls during planning] → Resolve deterministically, deduplicate identical references, and retain digest results within one plan operation.

## Migration Plan

1. **P0 execution truth:** ship semantic validation, manual-job operation semantics, full runtime image binding, and the deliberate compatibility-corpus refreeze. Require focused tests before continuing.
2. **P0 recovery truth:** write state-machine manifests, switch rollback and retention to their separate classifiers, centralize gate-aware recovery, and implement secret-generation transactions. Require crash/failure-injection tests before continuing.
3. **P1 adapter contract:** normalize global config, Run-tier targeting, finite/streaming envelopes, cancellation, and no-op behavior through shared helpers.
4. Update documentation, run the full Go suite, strict OpenSpec validation, static analysis, and the end-to-end harness before release.

Because the product is unreleased, rollback of this code change is a source
rollback; no persisted user-data migration is introduced. Development state
created by another contract version must be recreated, and older binaries are
not supported against the new release store.
