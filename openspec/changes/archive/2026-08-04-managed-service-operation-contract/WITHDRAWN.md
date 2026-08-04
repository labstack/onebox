# Withdrawn, not completed

This change was never implemented. It is archived here unedited because a
withdrawn design is still worth being able to read back to.

**Do not treat anything in this directory as a shipped requirement.** No task
in `tasks.md` was completed, and archiving it through `openspec archive` would
have promoted its requirements into `openspec/specs/` as though they had been
delivered. They were not.

## Why it was withdrawn

Its premise no longer holds. The proposal opens with "Onebox can classify
PostgreSQL, MySQL, Redis, and generic Compose services, but it does not own
their definitions" — the classifier is gone, and Onebox now does own them.

Two of its central commitments were reversed by product direction:

- **"Make MCP the primary product surface."** The MCP server was removed. It
  was read-only, so every mutation already went through the CLI, and an agent
  able to run `ob deploy` in a shell was never constrained by a read-only tool
  list.
- **A managed-service declaration layered onto the classifier contract.** The
  classifier contract was replaced wholesale by `adopt-declarative-project-schema`.

## What it proposed that now ships

Delivered by `adopt-declarative-project-schema`, against the declarative
contract rather than this one:

- an application-scoped, release-independent Compose project per service
- a stable network identity and a persistent-volume contract
- exclusive ownership: one driver renders a service, and a service identifier
  cannot collide with a workload
- a driver settings contract, applied through each driver's own mechanism, and
  refused where a driver has no mechanism that can apply it safely
- convergence under the lock, fence and journal regime
- credentials generated on the target, absent from the project, the generated
  runtime and its digest

## What it proposed that is still unbuilt

These are real and still worth doing. They should be proposed fresh against the
declarative contract, not resurrected from here:

- versioned driver contracts, so an Onebox upgrade cannot silently change an
  existing service's effective image or settings
- effective-value origin reporting (`invariant`, `user`, `profile`, `upstream`)
- digest-sealed managed-service plans, separate from deploy plans
- desired-versus-applied drift observation
- explicit secret projection: a service receives only the keys it was given,
  never the application's whole secret environment
- backup, restore, and protection evidence — which the shipped contract still
  declares and does not perform
