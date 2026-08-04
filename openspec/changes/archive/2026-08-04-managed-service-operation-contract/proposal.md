## Why

Onebox can classify PostgreSQL, MySQL, Redis, and generic Compose services, but it does not own their definitions or understand their lifecycle beyond generic accessory convergence. Managed data services need the same deterministic, health-gated, journaled ownership pattern already proven by managed Traefik, while preserving Onebox's plan binding, approval, and honest recovery boundaries.

## What Changes

- Add an optional, additive managed-service declaration for supported service components. Existing user-owned Compose services remain unchanged when management is not requested.
- Give each driver a versioned settings contract with typed common settings, typed service-specific settings, and a bounded native-parameter escape hatch. Driver-owned invariants cannot be overridden through settings.
- Resolve defaults deterministically by driver contract version, expose every effective value and its origin (`invariant`, `user`, `profile`, or `upstream`) in configuration and plans, and prevent a Onebox upgrade from silently changing an existing component's effective image or settings.
- Establish exclusive ownership rules: a managed component is rendered by exactly one registered Onebox driver and cannot collide with a user-authored Compose service.
- Give each managed component an application-scoped, release-independent Compose project, configuration payload, stable network identity, applied-state marker, and persistent-volume contract.
- Add executable, expiring, digest-sealed managed-service plans that bind desired configuration, pinned image, target, observed service state, and persistent-volume identity.
- Require approval according to environment policy and operation risk before managed-service mutation.
- Converge managed services under the existing lock, fence, journal, cancellation, event, and redaction regime; record the applied digest only after driver verification succeeds.
- Add structured managed-service observation for health, desired/applied drift, image identity, service version, persistence identity, completeness, and warnings.
- Make MCP the primary product surface: expose driver and settings discovery, structured change proposals, approval handoff, approved execution, task progress, cancellation, and final observation as schema-validated tools designed for LLM use.
- Let an agent persist accepted structured intent through a revision-bound local project-change tool, then require a fresh runtime proposal from the resulting configuration; no human YAML editing or silent production-only desired state is required.
- Keep the CLI as a thin adapter over the same operations service for tests, support, and break-glass recovery; no managed-service behavior may exist only in a manual CLI workflow.
- Make application deployment verify managed-service prerequisites without implicitly creating, upgrading, downgrading, or rolling back a managed service.
- Add explicit secret projection so a managed service receives only declared secret keys, never the application's complete secret environment.
- Keep provider-specific provisioning, backup, restore, upgrade, and protection evidence out of this foundation change. PostgreSQL and Redis drivers will be proposed separately against this contract.

## Capabilities

### New Capabilities

- `managed-service-definition`: Opt-in component ownership, driver selection, versioned settings and defaults, generated-service isolation, stable storage/network identity, and secret projection.
- `managed-service-planning`: Immutable managed-service plans, state bindings, image pinning, risk classification, approval, drift rejection, and deploy prerequisites.
- `managed-service-convergence`: Lock/fence/journal-governed staging and convergence with idempotency, atomic configuration publication, verification, and failure semantics.
- `managed-service-observation`: Structured health, version, image, persistence, completeness, and desired-versus-applied drift reporting.
- `managed-service-agent-interface`: LLM-oriented discovery, proposal, approval, execution, progress, error, and context-safety contracts over MCP.

### Modified Capabilities

None. This change introduces managed-service capabilities and does not modify
the existing `release-versioning` capability.

## Impact

- Project schema and normalization: `internal/config/schema.cue`, `internal/config/config.go`.
- Compose project construction, classification, rendering, network injection, payload staging, image handling, and redaction: `internal/compose`.
- Canonical operations, executable plan schemas, approval binding, execution dispatch, proposal safety, and observation: `internal/onebox`.
- Locking, fencing, journaling, convergence, status, and remote layout: `internal/engine`, `internal/journal`, `internal/release` or a new managed-service package.
- MCP tools and transport-safe schemas: `internal/mcp`.
- Thin test, support, and break-glass CLI adapters: `cmd/ob`.
- Contract, engine, execution-boundary, redaction, cancellation, and opt-in Docker end-to-end tests.
- Documentation status and authority: current guides describe only implemented behavior and this active OpenSpec change remains the normative proposed contract until archive.
- Documentation for the additive v1 authoring contract and the boundary between declared, Compose-owned, and Onebox-managed services.
