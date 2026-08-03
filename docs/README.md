# Onebox documentation

Onebox documentation distinguishes shipped behavior from proposed behavior.
This distinction is part of the product safety contract: an agent must not
treat a roadmap statement as an executable capability.

## Authority and status

| Status | Authority | Purpose |
|---|---|---|
| Normative | Archived capability contracts under [`openspec/specs/`](../openspec/specs/) | Durable requirements for completed OpenSpec changes |
| Implemented | [`README.md`](../README.md), [`schema-v1.md`](schema-v1.md), and [`mcp.md`](mcp.md), checked against code and tests | What the current binary accepts and does |
| Proposed | Active changes under [`openspec/changes/`](../openspec/changes/) | Normative requirements, design, and implementation tasks for work not yet shipped |
| Product | [`product.md`](product.md) | Stable product direction and boundaries; never an implementation claim by itself |

When documents conflict:

1. Archived OpenSpec capability contracts define required shipped behavior.
2. Current code, schemas, and tests determine what the installed binary does;
   disagreement with a capability contract is a defect, not a second contract.
3. Current user guides describe that implemented behavior and must be corrected
   if they disagree with it.
4. An active OpenSpec change is authoritative only for the proposed change it
   names. It does not make that behavior available.
5. Product direction provides context, not an executable contract.

## Current documentation

- [`README.md`](../README.md): installation, current capabilities, and the
  supported single-host envelope.
- [`schema-v1.md`](schema-v1.md): accepted `onebox.run/v1` authoring contract as
  the binary parses it today. The active `adopt-declarative-project-schema`
  change redefines that identity; until it archives, this file is what ships.
- [`mcp.md`](mcp.md): current MCP tools and their safety boundary. These ship
  today; product direction has since withdrawn MCP as the product interface, and
  this guide will be archived when that change lands.
- [`product.md`](product.md): product direction — the ownership boundary, the
  one-application-per-host scope, and the CLI as the interface.
- [`archive/`](archive/): superseded documents, kept unedited so a decision can
  be read back to the reasoning that produced it. Never authoritative.

## Active OpenSpec changes

- [`managed-service-operation-contract`](../openspec/changes/managed-service-operation-contract/):
  proposed generic contract for managed supporting services. It defines typed
  settings, visible defaults, explicit image selection, sealed plans, durable
  operations, approval, convergence, observation, and the LLM-facing MCP
  interface. It is not implemented yet.

Provider-specific PostgreSQL and Redis enablement, adoption, detach/destroy,
backup/restore, and major-version upgrades require separate future OpenSpec
changes. The generic contract does not silently authorize them.

## Canonical capability specifications

- [`release-versioning`](../openspec/specs/release-versioning/spec.md): shipped
  CalVer identity, build provenance, minimum-runner ordering, and guarded
  release-tag creation.

## Documentation workflow

Behavioral changes follow the OpenSpec lifecycle:

1. Create a proposal and delta specifications.
2. Resolve design and safety decisions.
3. Review the implementation checklist.
4. Apply the change with tests and fault-oriented verification.
5. Update the current schema and MCP guides only for behavior that shipped.
6. Strict-validate and archive the change so its specifications become durable
   project truth.

Every public capability statement must be labeled as implemented or proposed.
Examples for proposed fields or tools must say that they are non-executable.
