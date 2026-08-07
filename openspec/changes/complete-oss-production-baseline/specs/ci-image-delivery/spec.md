## Purpose

Defines an official CI handoff that produces immutable workload images and enters the canonical Onebox plan-and-execute path without creating a competing deployment implementation.

## ADDED Requirements

### Requirement: CI resolves build sources to immutable images

The official workflow SHALL build each selected build-sourced workload, push
it to a configured registry, resolve the registry-confirmed digest, and supply
that digest to the canonical Onebox planning command. A mutable tag alone SHALL
NOT be accepted as the workflow result.

#### Scenario: Image is built and pushed
- **GIVEN** a build-sourced workload and valid registry credentials
- **WHEN** the workflow completes its build phase
- **THEN** it records a workload-to-digest mapping consumable by `ob plan`

#### Scenario: Registry digest cannot be resolved
- **GIVEN** a pushed tag whose immutable digest cannot be confirmed
- **WHEN** planning would begin
- **THEN** the workflow fails before contacting the deployment target and names the failed workload

### Requirement: CI uses the canonical lifecycle

The workflow SHALL invoke the shipped CLI for validation, planning, approval
validation, and deployment. It SHALL NOT reproduce rendering, drift checks,
approval minting, locking, fencing, journaling, rollback, or recovery logic in
workflow scripts or actions.

#### Scenario: Deploy is authorized
- **GIVEN** an executable plan and an independently delivered matching approval artifact
- **WHEN** the deploy job runs
- **THEN** it invokes `ob deploy` with those artifacts and publishes the terminal structured result

#### Scenario: Approval is absent
- **GIVEN** a plan requiring approval
- **WHEN** CI has no independently delivered matching grant
- **THEN** the workflow stops after planning and does not mint or infer approval from workflow text, actor identity, or model output

### Requirement: CI artifacts are secret-safe and retryable

Workflow artifacts SHALL include only redacted validation output, immutable
image mappings, executable plans, and structured operation results. Registry,
SSH, backup, application, and approval-delivery secrets SHALL remain in the CI
secret facility and SHALL NOT be uploaded as artifacts. A retry SHALL reuse
the exact plan only while it remains valid and state-bound; otherwise it SHALL
re-plan and require a new approval.

#### Scenario: Retry after runner loss
- **GIVEN** CI lost its connection after deployment began
- **WHEN** the job is retried
- **THEN** it inspects the operation identity and resumes or reports the terminal state rather than blindly starting another deployment

#### Scenario: Plan expired
- **GIVEN** an expired executable plan and approval
- **WHEN** the workflow retries
- **THEN** it regenerates the plan and refuses to reuse the old approval

### Requirement: CI output contracts are versioned

The official workflow SHALL expose a versioned result containing source
revision, workload image digests, plan identity, operation identity when one
exists, terminal state, and typed secret-free failure. Changes incompatible
with consumers SHALL receive a new schema version.

#### Scenario: Consumer reads workflow result
- **GIVEN** a completed plan-only or deployment workflow
- **WHEN** a downstream job reads its result
- **THEN** it can branch on schema version and terminal code without parsing human logs

