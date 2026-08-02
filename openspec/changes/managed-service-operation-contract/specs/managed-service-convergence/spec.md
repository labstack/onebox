## Purpose

Defines crash-consistent, idempotent, health-gated execution semantics for applying a sealed managed-service plan without risking persistent data.

## ADDED Requirements

### Requirement: Execution revalidates under exclusive authority
Execution SHALL acquire the application lock, establish a new fence epoch, start heartbeat maintenance, and re-observe every plan binding before staging or mutation. Every mutating host command SHALL be fence-guarded, and loss of authority SHALL halt the operation.

#### Scenario: Concurrent operation holds the lock
- **WHEN** another live operation owns the application lock
- **THEN** managed-service execution refuses without staging or mutation

#### Scenario: Previous runner becomes stale
- **WHEN** a newer operation establishes a fence epoch while an older runner remains alive
- **THEN** the older runner's next mutating command fails host-side before changing service state

#### Scenario: State changes while waiting for the lock
- **WHEN** the target changes between planning and locked re-observation
- **THEN** execution rejects the stale plan before publishing staged configuration

### Requirement: Configuration publication is deterministic and atomic
The system SHALL render and hash the complete service payload deterministically, upload it only to an operation-scoped staging directory, validate it before publication, and replace live configuration atomically. Stale staging directories SHALL be safely ignored or cleaned under lock. Secret files SHALL use restrictive permissions and SHALL never be interpolated into host commands.

#### Scenario: Upload is interrupted
- **WHEN** execution stops during payload upload
- **THEN** the live service configuration remains unchanged and a retry does not treat the partial staging directory as applied

#### Scenario: Rendered payload is unchanged
- **WHEN** desired and applied payload digests match and actual service state passes verification
- **THEN** execution records a no-op without uploading, restarting, or recreating the service

#### Scenario: Payload validation fails
- **WHEN** driver or Compose validation rejects the staged payload
- **THEN** execution stops before replacing live configuration or touching the running container

### Requirement: Driver-classified convergence is minimal
The executor SHALL perform only the action authorized by the plan's driver classification: no-op, live reload, restart, recreation with attached persistent resources, or a separate dedicated operation. It SHALL re-check the actual container and service state rather than trusting disk markers alone.

#### Scenario: Live state differs despite matching applied marker
- **WHEN** the applied digest matches desired state but the running image, mounts, network, or service version differs
- **THEN** execution does not report a no-op and follows the plan's drift refusal or authorized convergence behavior

#### Scenario: Restart is sufficient
- **WHEN** the sealed plan authorizes a restart-class change
- **THEN** execution preserves container identity where supported, persistent resources, and network identity while performing no broader action

#### Scenario: Upgrade is required
- **WHEN** actual or desired state implies an upgrade-only transition
- **THEN** ordinary convergence halts without replacing the running service

### Requirement: Success is health-gated and evidence-backed
Execution SHALL run the driver verification contract after convergence and SHALL write the applied digest and successful terminal journal record only after verification succeeds. Verification SHALL be bounded by context and explicit timeouts. Applied evidence SHALL include the resulting immutable image, service version, volume identities, and driver-safe verification facts.

#### Scenario: Service becomes healthy
- **WHEN** the authorized action completes and all driver verification checks succeed before their deadlines
- **THEN** the system atomically records applied state, appends successful evidence, and emits a terminal success event

#### Scenario: Container starts but verification fails
- **WHEN** the container is running but health, connectivity, version, mount, or driver-specific verification fails
- **THEN** the system does not write the desired applied digest and records a failed operation with safe diagnostic guidance

#### Scenario: Execution is canceled during verification
- **WHEN** the operation context is canceled while waiting for verification
- **THEN** execution stops promptly, releases authority through bounded cleanup, and leaves the operation retryable rather than successful

### Requirement: Failure preserves recoverability
The executor SHALL never delete, reinitialize, replace, or detach a persistent volume during ordinary apply. It SHALL retain the last known-good configuration artifact and record whether the running service is old, new, or indeterminate after failure. Automatic configuration rollback SHALL occur only when the sealed plan and driver classify it as safe; otherwise the system SHALL halt with explicit recovery guidance.

#### Scenario: Recreation fails with an existing data volume
- **WHEN** a container recreation fails after the previous container stops
- **THEN** the volume remains attached or recoverably identifiable, no initialization is attempted, and the journal records the observed post-failure state

#### Scenario: Safe configuration rollback is supported
- **WHEN** verification fails and the sealed plan authorizes driver-verified configuration rollback
- **THEN** execution may restore the previous configuration artifact, reconverge, verify it, and record both the failed attempt and rollback result

#### Scenario: Rollback safety is unknown
- **WHEN** verification fails and the driver cannot prove configuration rollback safe
- **THEN** execution preserves evidence and persistent resources and halts instead of guessing

### Requirement: Execution emits redaction-safe ordered evidence
Managed-service execution SHALL append intent, authority, staging, convergence, verification, result, and failure facts to the operation journal and structured event stream in deterministic order. External outputs SHALL contain bounded, typed, redaction-safe facts rather than arbitrary command output or service logs.

#### Scenario: Driver command returns sensitive stderr
- **WHEN** a driver subprocess or container command fails with arbitrary output
- **THEN** trusted local diagnostics may retain bounded detail while model-facing events and observations emit a stable error code and safe guidance without the raw output
