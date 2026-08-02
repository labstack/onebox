## Review outcome

Approved as an implementation-ready foundation design. This is not approval to
enable PostgreSQL, Redis, or any other production provider; each provider still
requires its own reviewed OpenSpec change and qualification evidence.

## Production design findings resolved

- Ownership is explicit: services remain Compose-owned unless a closed managed
  declaration selects one built-in driver contract. Collisions, implicit
  adoption, detach, and deletion all fail closed.
- Version selection is explicit at three levels: driver contract, immutable
  profile, and service image. Floating or unqualified images are rejected and
  executable plans bind the resolved digest and platform.
- Settings are flexible without becoming an invariant bypass. Drivers expose
  typed fields plus bounded native parameters, while storage, identity,
  networking, health, secrets, and control metadata stay protected.
- Defaults are deterministic and visible. Every effective controlled value has
  an `invariant`, `user`, `profile`, or `upstream` origin; changing profile
  semantics requires a new profile identifier.
- Application and managed-service lifecycles are independent. Managed services
  have stable projects, networks, volumes, immutable revisions, and applied
  evidence that application deploy, rollback, and retention cannot mutate.
- Planning binds desired, applied, and actual evidence and refuses incomplete
  observation, repointed tags, secret-source changes, stale approvals, and
  unsupported upgrade or destructive transitions.
- Execution reuses lock, fence, lease, journal, cancellation, verification, and
  redaction authorities. Applied state is committed only after bounded driver
  verification succeeds.
- Long-running MCP mutations have durable idempotent operation identity,
  authoritative polling, restart reconstruction, and cooperative cancellation.
- LLM-first configuration is complete: typed intent produces a revision-bound
  semantic project change; a constrained local tool persists only that exact
  change; runtime planning then restarts from durable desired state.
- Secrets are projected per driver slot and never accepted as model-visible
  tool arguments or broad application environment injection.

## Required implementation gates

- Build the canonical Go service before MCP and non-primary CLI adapters.
- Keep every production provider disabled while validating the generic
  substrate with a non-production test driver.
- Complete contract, property, fuzz, race, redaction, fault-injection,
  crash-recovery, cancellation, and Docker-backed end-to-end tests.
- Keep adoption, detach, destroy, backup, restore, scheduling, and provider
  upgrades in separate changes with explicit destructive and data-integrity
  contracts.
- Do not describe a provider as production-ready until its own backup, restore,
  upgrade, failure-mode, and data-integrity gates pass.

## Residual risks

- The foundation is broad. Implement it in vertical slices and validate the
  driver abstraction immediately against the first provider rather than
  generalizing ahead of evidence.
- A local operation repository is suitable for development and break-glass
  recovery, not authenticated team approval. Production mutation remains
  disabled until a trusted approval provider is configured.
- Single-host operation cannot provide host-level high availability. The
  product must continue to state that boundary explicitly.
