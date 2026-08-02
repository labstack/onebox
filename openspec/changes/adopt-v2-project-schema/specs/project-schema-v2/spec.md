## Purpose

Defines the `onebox.run/v2` authoring contract, in which the project file states
intent and Onebox derives the runtime, so that declaring an application is short,
its ownership boundary is explicit in the shape of the file, and every accepted
project normalizes to one canonical form that later phases can bind.

## ADDED Requirements

### Requirement: Projects declare a supported schema identity

A project SHALL declare `schema: onebox.run/v2`. Onebox SHALL reject a project
whose schema identity is absent, malformed, or unsupported by the running binary
before performing validation, contacting a target, or generating a runtime, and
SHALL name both the declared identity and the identities the binary supports.

#### Scenario: Unsupported schema identity
- **WHEN** a project declares a schema identity the running binary does not support
- **THEN** loading fails, no target connection is attempted, and the error names the declared identity and the supported identities

#### Scenario: Missing schema identity on a v2-shaped project
- **WHEN** a project omits the schema identity but uses v2 blocks
- **THEN** loading fails and the error states that the schema identity is required

### Requirement: A minimum project is an identifier, a server, and one workload source

Onebox SHALL accept a project that declares only an application identifier, a
server for the selected environment, and exactly one workload source, where a
workload source is a build context, an image reference, or a bounded Compose
reference. Every other field SHALL resolve from a documented default, and the
resolved value's origin SHALL be reported as a default rather than presented as
user intent.

#### Scenario: Minimum project validates
- **WHEN** a project declares an application identifier, a server, and a single build context
- **THEN** validation succeeds and the normalized project contains one workload named for the application

#### Scenario: No workload source
- **WHEN** a project declares neither a build context, an image reference, nor a Compose reference for any workload
- **THEN** validation fails and the error states that at least one workload source is required

#### Scenario: Default origin is reported
- **WHEN** a field resolves from a documented default rather than the project file
- **THEN** the normalized configuration reports that value's origin as a default

### Requirement: Workloads and services state the ownership boundary

A project SHALL express containers the user owns as workloads and driver-backed
supporting services as services. A workload SHALL declare exactly one source: a
build context, an image reference, or a bounded Compose reference. A container
that is neither built by the user nor backed by a driver SHALL be expressible as
a workload by image reference, and SHALL NOT require a separate category.

#### Scenario: Third-party container without a driver
- **WHEN** a project declares a workload whose only source is a third-party image reference
- **THEN** validation succeeds and the workload is treated like any other user-owned container

#### Scenario: Workload declares two sources
- **WHEN** a workload declares both a build context and an image reference
- **THEN** validation fails and the error states that a workload has exactly one source

### Requirement: Shorthand expands deterministically into one canonical form

The contract SHALL accept shorthand forms, and every accepted project SHALL
normalize to exactly one canonical form. A scalar service declaration SHALL
expand to the equivalent object declaration. Workload fields declared at the top
level SHALL expand to a single workload named for the application. Expansion
SHALL be deterministic: the same project text SHALL always produce the same
canonical form, and the canonical form SHALL be printable.

#### Scenario: Scalar service shorthand
- **WHEN** a project declares a service as a scalar version
- **THEN** the canonical form contains the equivalent object declaration with that version

#### Scenario: Top-level workload shorthand
- **WHEN** a project declares workload fields at the top level and declares no workload block
- **THEN** the canonical form contains exactly one workload named for the application carrying those fields

#### Scenario: Shorthand and explicit form conflict
- **WHEN** a project declares workload fields at the top level and also declares a workload block
- **THEN** validation fails and the error names both locations

#### Scenario: Expansion is deterministic
- **WHEN** the same project text is normalized more than once
- **THEN** every canonical form produced is byte-identical

### Requirement: The schema is closed and correction hints are offered

Validation SHALL reject unknown fields, unknown enum values, and values that
violate declared scalar constraints. When a rejected name is close to an accepted
name in the same position, the error SHALL name the accepted alternative.
Validation SHALL report the location of each violation in the source file, and
SHALL NOT expose the internal schema language to the user.

#### Scenario: Unknown field
- **WHEN** a project declares a field the contract does not define
- **THEN** validation fails, the error names the unknown field and its location, and no runtime is generated

#### Scenario: Near-miss field name
- **WHEN** a rejected field name differs slightly from an accepted field name valid in that position
- **THEN** the error names the accepted alternative

#### Scenario: Multiple violations
- **WHEN** a project contains more than one violation
- **THEN** validation reports every violation rather than stopping at the first

### Requirement: Raw Compose is bounded to workloads

A workload MAY reference a named service in a user-authored Compose file as its
source. A service declaration SHALL NOT accept a Compose reference or any raw
runtime override. A referenced Compose service that does not exist, or a
reference from a service declaration, SHALL fail validation. A data service the
user authors in Compose SHALL be expressible only as a workload, and Onebox SHALL
NOT report it as managed.

#### Scenario: Workload references a Compose service
- **WHEN** a workload references a named service that exists in the referenced Compose file
- **THEN** validation succeeds and the workload's source is that Compose service

#### Scenario: Reference names a missing Compose service
- **WHEN** a workload references a service name absent from the referenced Compose file
- **THEN** validation fails and the error names the missing service and the file

#### Scenario: Service declaration attempts a Compose reference
- **WHEN** a service declaration carries a Compose reference
- **THEN** validation fails and the error states that services are driver-owned and a user-authored data service must be declared as a workload

### Requirement: Service declarations are inert in this contract

A service declaration SHALL validate and normalize, and SHALL NOT cause Onebox to
create, converge, tune, back up, upgrade, or otherwise operate that service.
Observation SHALL report a declared service as declared and not managed until a
driver capability establishes otherwise.

#### Scenario: Declared service is not provisioned
- **WHEN** a project declares a service and a runtime is generated
- **THEN** no container, volume, or network is created for that service by this contract

#### Scenario: Declared service is honestly reported
- **WHEN** observation reports a declared service
- **THEN** it is reported as declared and not managed, and no backup or restore guarantee is implied

### Requirement: A machine-readable schema is published for editors

Each release SHALL publish a JSON Schema describing the accepted contract,
derived from the same source that validates projects, so that the published
schema and the enforced contract cannot diverge within a release. Scaffolding a
project SHALL write a reference to that schema into the project file.

#### Scenario: Published schema matches the enforced contract
- **WHEN** the published schema for a release is compared against the contract that release enforces
- **THEN** they accept and reject the same projects

#### Scenario: Scaffolded project references the schema
- **WHEN** a project is scaffolded
- **THEN** the written project file carries a reference to the published schema for that release

### Requirement: Authoring surfaces are agent-operable

Commands that validate a project or print its configuration SHALL support a
versioned structured output mode. In that mode a failure SHALL emit a typed
error code, a message free of secrets, the source location where one applies,
and the command that resolves the failure. Diagnostics SHALL NOT be written to
the structured output stream.

#### Scenario: Structured validation failure
- **WHEN** validation fails and structured output is requested
- **THEN** the output carries a versioned envelope, a typed error code, a source location, and a resolving command

#### Scenario: Structured output stays machine-readable
- **WHEN** structured output is requested and local diagnostics are produced
- **THEN** diagnostics are written to the diagnostic stream and the structured stream remains parseable

### Requirement: Secrets never enter authoring output

Normalized configuration, validation errors, and published schema output SHALL
NOT contain plaintext secret values. A value sourced from a secret provider or an
environment file SHALL be represented by its reference, never by its resolved
content.

#### Scenario: Secret-valued field in normalized output
- **WHEN** normalized configuration is printed for a project whose workload consumes secret values
- **THEN** the output contains the references and no resolved secret content

#### Scenario: Validation error near a secret value
- **WHEN** validation fails on a field adjacent to a secret value
- **THEN** the error text contains no secret content

### Requirement: YAML is the only authoring format

The contract SHALL accept projects authored in YAML only. A project authored in
the internal validation language SHALL be rejected with an error naming the
conversion command, so that the validation layer remains an implementation detail
rather than a second documented authoring surface.

#### Scenario: Project authored in the validation language
- **WHEN** a project is presented in the internal validation language rather than YAML
- **THEN** loading fails and the error names the command that converts it to a v2 YAML project
