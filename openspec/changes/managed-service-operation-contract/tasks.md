## 1. Contract and configuration foundation

- [ ] 1.1 Add the common managed-service configuration envelope, persistence references, resource controls, secret-slot references, and explicit ownership mode to the Go configuration model without changing existing Compose-owned behavior.
- [ ] 1.2 Extend the CUE schema with a closed managed-service union keyed by versioned driver contracts and reject ownership collisions, unknown fields, floating image references, and protected-invariant overrides.
- [ ] 1.3 Add normalization and validation tests covering existing Compose-owned components, managed declarations, missing drivers, invalid profiles, invalid resources, and removal of a previously managed declaration.
- [ ] 1.4 Version the normalized configuration and runner capability documents so an unsupported managed-service schema or driver fails before planning or remote mutation.

## 2. Driver registry and effective settings

- [ ] 2.1 Define the built-in driver interface for catalog metadata, typed validation, default resolution, rendering, observation, change classification, health verification, and redaction.
- [ ] 2.2 Implement a registry that resolves immutable driver and profile identifiers, reports runner capabilities, and refuses duplicate or dynamically downloaded driver implementations.
- [ ] 2.3 Implement deterministic effective-setting resolution with invariant, explicit, profile, and upstream origins, preserving upstream-owned unset values instead of guessing them.
- [ ] 2.4 Implement the bounded native-parameter escape hatch with driver allowlists, canonical string values, protected keys, and explicit apply-effect metadata.
- [ ] 2.5 Add a non-production test driver and table-driven tests for setting origins, profile immutability, native-parameter boundaries, effect classification, and redaction.

## 3. Stable runtime identity and deterministic rendering

- [ ] 3.1 Add deterministic helpers for service project names, the shared services network, revision directories, applied-state paths, and labeled volume names, including length and character validation.
- [ ] 3.2 Render managed services into an independent Compose project and stable network so application release rollback cannot recreate or remove managed-service containers or volumes.
- [ ] 3.3 Render immutable revision payloads from canonical inputs, validate them before publication, and make identical inputs produce the same payload digest.
- [ ] 3.4 Add golden and property tests for naming, rendering, collision detection, revision determinism, network attachment, and the invariant that lifecycle commands never include volume deletion.

## 4. Observation and completeness

- [ ] 4.1 Define desired, applied, and actual managed-service state models with explicit evidence completeness and bounded, typed error details.
- [ ] 4.2 Implement strictly read-only collection of container, image, health, network, volume, revision, orphaned Onebox-labeled resources, and driver-specific evidence without invoking convergence paths.
- [ ] 4.3 Compute three-way drift and effective-setting origins while distinguishing absent, unknown, incomplete, unhealthy, and in-sync states.
- [ ] 4.4 Add tests for partial remote failures, unreadable applied state, missing containers, runtime drift, bounded output, and secret-free observation serialization.

## 5. Sealed planning and safety classification

- [ ] 5.1 Add a versioned managed-service plan schema that binds normalized intent, effective settings and origins, secret-source revisions, pinned image digest, network and volume identity, applied and actual evidence, driver versions, and completeness.
- [ ] 5.2 Resolve image tags to immutable digests during planning and fail closed when resolution, evidence collection, driver compatibility, or state binding is incomplete.
- [ ] 5.3 Produce a minimal resolved diff, driver-classified action, risk level, approval requirement, expected effects, verification contract, expiry, and sealed plan digest.
- [ ] 5.4 Reject execution of missing, edited, expired, already-consumed, incompatible, or stale plans and ensure force flags cannot bypass destructive, upgrade, or incomplete-evidence gates.
- [ ] 5.5 Add deterministic plan fixtures and tests for no-op, restart, recreate, upgrade-required, destructive-refused, stale-state, changed-secret, repointed-tag, and incomplete-observation cases.

## 6. Durable operation and approval boundary

- [ ] 6.1 Introduce durable operation identifiers, explicit proposal and execution states, idempotency keys, ordered evidence references, and legal state-transition validation.
- [ ] 6.2 Implement a local operation repository using private permissions, atomic file replacement, advisory locking, corruption detection, and restart-safe reconstruction.
- [ ] 6.3 Add an approval-provider interface that binds an out-of-band approval capability to the exact sealed plan, actor, expiry, and single execution attempt.
- [ ] 6.4 Ensure model-authored text and ordinary tool arguments can never mint approval or supply plaintext secrets, and return a typed handoff when trusted interaction is required.
- [ ] 6.5 Add repository and approval tests for concurrent retries, duplicate idempotency keys, crash recovery, tampering, expiry, replay, actor mismatch, and redaction.

## 7. Crash-consistent convergence engine

- [ ] 7.1 Add managed-service operation and step kinds to the canonical engine, with lock, fence token, lease heartbeat, cancellation checkpoints, and revalidation after authority is acquired.
- [ ] 7.2 Implement atomic remote revision staging, validation, activation, and applied-state publication using the existing journal as the authority for completed external effects.
- [ ] 7.3 Execute only the driver-classified minimal action, then require bounded health and driver verification evidence before committing the applied marker.
- [ ] 7.4 Preserve prior revisions and data volumes on every failure path, permit automatic rollback only when the driver proves it safe, and otherwise stop with explicit recovery guidance.
- [ ] 7.5 Add fault-injection tests at every state transition, including upload interruption, lock loss, runner crash, cancellation, verification failure, partial recreation, retry, and stale-worker fencing.

## 8. Secret projection and logging hygiene

- [ ] 8.1 Extend secret handling to project only declared secret slots into per-service files with private permissions and without exposing broad application environments.
- [ ] 8.2 Bind plans to encrypted-source revisions or equivalent non-plaintext identities so a secret change invalidates approval without storing or hashing plaintext credentials.
- [ ] 8.3 Apply centralized structured redaction, output length limits, and safe error mapping to driver commands, journals, operation records, observations, and audit evidence.
- [ ] 8.4 Add adversarial tests using credentials in stdout, stderr, environment values, file paths, Compose payloads, and driver errors to prove no public surface leaks them.

## 9. LLM-first MCP product interface

- [ ] 9.1 Add a paginated `onebox_service_catalog` read tool that returns compact driver, profile, setting, default-origin, constraint, and apply-effect metadata with opaque resource links for detail.
- [ ] 9.2 Extend `onebox_observe` with compact managed-service summaries and opt-in bounded detail while preserving its strictly read-only behavior.
- [ ] 9.3 Add `onebox_propose_service_change` with a closed structured-intent schema, request idempotency, dry-run semantics, sealed-plan creation, and no arbitrary YAML, shell, approval, or secret fields.
- [ ] 9.4 Add `onebox_apply_project_change` to atomically apply only an exact revision-bound semantic patch to the configured project file, preserve unrelated YAML, validate the result, avoid target access, and return an idempotent new revision.
- [ ] 9.5 Add `onebox_get_operation`, `onebox_execute_approved_operation`, and `onebox_cancel_operation` over durable operation IDs so disconnects and missed notifications do not lose work.
- [ ] 9.6 Publish accurate MCP input and output schemas, annotations, OAuth audience checks for remote use, typed bounded errors, pagination, and URL-based trusted handoffs for approval or secret entry.
- [ ] 9.7 Add MCP conformance tests for schema validation, structured output, local project-mutation isolation, annotations, read-only guarantees, retry idempotency, polling, cancellation, context-size limits, and approval forgery attempts.

## 10. Application integration and non-primary adapters

- [ ] 10.1 Change application deploy planning to bind managed-service readiness and network facts as prerequisites and refuse deploy when a required service is absent, unhealthy, incomplete, or unconverged.
- [ ] 10.2 Ensure deploy, rollback, and app cleanup never implicitly mutate, recreate, detach, or remove managed services, their revisions, networks, or data volumes.
- [ ] 10.3 Add a thin CLI adapter for catalog, observe, propose, poll, execute-approved, and cancel operations that invokes the canonical service contract and contains no unique lifecycle logic.
- [ ] 10.4 Add integration tests proving MCP and CLI produce equivalent plans and operation states and that application rollback leaves managed-service identities and data untouched.

## 11. Controlled rollout and production gates

- [ ] 11.1 Ship the generic framework disabled for production providers, enable only the non-production test driver, and document the feature and compatibility gates.
- [ ] 11.2 Run unit, race, fuzz, schema, redaction, crash-recovery, and Docker-backed end-to-end suites across clean install, retry, drift, restart, cancellation, and rollback scenarios.
- [ ] 11.3 At implementation completion, update current product, schema, security, operations, and agent-facing guides only for shipped behavior; preserve proposed labels for unavailable providers and document ownership, defaults, discovery, approval, recovery, and the Compose-owned escape hatch.
- [ ] 11.4 Specify adoption, detach, destroy, backup/restore, PostgreSQL upgrade, and Redis persistence as separate future OpenSpec changes rather than adding unsafe implicit behavior to this contract.
- [ ] 11.5 Complete a threat-model and failure-mode review, validate all OpenSpec artifacts strictly, and keep PostgreSQL and Redis production enablement blocked until their individual contracts and qualification suites are approved.
