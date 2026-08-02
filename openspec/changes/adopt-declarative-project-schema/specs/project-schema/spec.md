## Purpose

Defines the `onebox.run/v1` authoring contract, in which the project file states
intent and Onebox derives the runtime, so that declaring an application is short,
its ownership boundary is explicit in the shape of the file, every operational
fact the project needs has a home, and the contract can grow for years without a
second schema version.

## ADDED Requirements

### Requirement: Projects declare a supported schema identity

A project SHALL declare `api_version: onebox.run/v1`. Onebox SHALL reject a
project whose schema identity is absent, malformed, or unsupported by the running
binary before performing validation, contacting a target, or generating a
runtime, and SHALL name both the declared identity and the identities the binary
supports.

#### Scenario: Unsupported schema identity
- **WHEN** a project declares a schema identity the running binary does not support
- **THEN** loading fails, no target connection is attempted, and the error names the declared identity and the supported identities

#### Scenario: Project written against the withdrawn classifier contract
- **WHEN** a project declares `onebox.run/v1` but uses the withdrawn `components` block
- **THEN** loading fails with an error naming the block and the authoring guide, rather than a generic unknown-field error

### Requirement: A minimum project is an identifier, a server, and one workload source

Onebox SHALL accept a project that declares only an application identifier, a
server for the selected environment, and exactly one workload source, where a
workload source is a build context, an image reference, or a bounded Compose
reference. Every other field SHALL resolve from a documented default, and the
resolved value's origin SHALL be reported as a default rather than as user
intent.

#### Scenario: Minimum project validates
- **WHEN** a project declares an application identifier, a server, and a single build context
- **THEN** validation succeeds and the normalized project contains one workload named for the application

#### Scenario: No workload source
- **WHEN** no workload declares a build context, an image reference, or a Compose reference
- **THEN** validation fails and the error states that at least one workload source is required

#### Scenario: Default origin is reported
- **WHEN** a field resolves from a documented default rather than the project file
- **THEN** the normalized configuration reports that value's origin as a default

### Requirement: Workloads and services state the ownership boundary

A project SHALL express containers the user owns as workloads and driver-backed
supporting services as services. A workload SHALL declare exactly one source. A
container that is neither built by the user nor backed by a driver SHALL be
expressible as a workload by image reference and SHALL NOT require a separate
category.

#### Scenario: Third-party container without a driver
- **WHEN** a project declares a workload whose only source is a third-party image reference
- **THEN** validation succeeds and the workload is treated like any other user-owned container

#### Scenario: Workload declares two sources
- **WHEN** a workload declares both a build context and an image reference
- **THEN** validation fails and the error states that a workload has exactly one source

### Requirement: The contract covers every operational fact the classifier contract expressed

The contract SHALL provide a home for each operational fact the withdrawn
classifier contract could express, so that restructuring removes no shipped
capability. It SHALL express, at minimum: release verification including expected
status codes, exact response headers, scalar JSON assertions, and expected
migration revisions; lifecycle hooks at bootstrap, pre-release, post-release, and
post-deploy; environment files and preflight checks asserting required and
present keys; deployment ordering; release retention; migration policy; outcome
notifications; registry pull credentials distinct from any publishing
configuration; proxy configuration including image, network, and configuration
source; per-workload rollout strategy, replica count, readiness, and drain
behavior; persistence mode and volume identity; declared protection and
observability posture; and environment policy including approval requirements,
minimum runner version, and migration backup requirements.

#### Scenario: Every classifier-contract project is expressible
- **WHEN** each of the organization's existing project files is expressed under this contract
- **THEN** every operational fact it declared has a home, and any that does not is a defect in this contract rather than an authoring error

#### Scenario: Verification assertions survive
- **WHEN** a project declares a verification step asserting a status code, an exact response header, and a scalar JSON value
- **THEN** all three are expressible and are carried into the normalized project

#### Scenario: Registry pull is distinct from publishing
- **WHEN** a project declares credentials for pulling images onto the host
- **THEN** they are expressed independently of any configuration describing where images are published

### Requirement: The contract grows additively without a second schema version

Schema evolution SHALL be additive under a stable identity. A later release MAY
add optional fields, additional enum members, and object forms of existing
scalars; it SHALL NOT remove a field, narrow an accepted value, or change the
meaning of an accepted project. A project that a release accepts SHALL continue
to be accepted, with unchanged meaning, by every later release carrying the same
identity.

Because an older binary cannot understand a newer field, an environment MAY
declare a minimum runner version, and a binary older than that SHALL fail closed
during validation rather than silently ignoring what it cannot represent.

#### Scenario: Later release adds an optional field
- **WHEN** a later release adds an optional field and a project omits it
- **THEN** the project's meaning is unchanged and it validates as before

#### Scenario: Older runner meets a project that requires a newer one
- **WHEN** a project declares a minimum runner version newer than the running binary
- **THEN** validation fails closed and names the required version, rather than proceeding with fields it cannot represent

#### Scenario: Narrowing is refused
- **WHEN** a change would cause a previously accepted project to be rejected or to mean something different
- **THEN** it is a breaking change to this contract and cannot ship under this identity

### Requirement: Fields that will grow accept a scalar shorthand and an object form

Every field whose future needs are already visible SHALL accept both a concise
scalar form and an object form, and the scalar SHALL expand into the object so
that later additions are optional keys rather than a shape change. This SHALL
apply at least to: the server for an environment, which also accepts a list; a
workload's build source; a workload's health check; a service declaration; and
the secret configuration, which accepts named sources rather than a single file.

#### Scenario: Scalar and object forms are equivalent
- **WHEN** a field is declared in its scalar form
- **THEN** the canonical form contains the equivalent object, and declaring that object directly produces an identical canonical form

#### Scenario: Environment accepts more than one server
- **WHEN** an environment declares a list of servers
- **THEN** the project validates and the normalized model carries them in declared order

#### Scenario: Health check declared as an executed command
- **WHEN** a workload declares a health check as an executed command rather than an HTTP path
- **THEN** the project validates and the check is carried as an exec form

### Requirement: Routing expresses multiple names and non-HTTP entrypoints

A workload SHALL be able to publish more than one domain, and SHALL be able to
publish an entrypoint that is not HTTP, including a port carrying a non-HTTP
protocol behind TLS. A single domain and a single port SHALL remain expressible
as scalars.

#### Scenario: Several domains on one workload
- **WHEN** a workload declares more than one domain
- **THEN** the project validates and every declared domain routes to that workload

#### Scenario: Non-HTTP entrypoint
- **WHEN** a workload publishes an entrypoint carrying a non-HTTP protocol
- **THEN** the project validates and the entrypoint's protocol is carried into the normalized project

#### Scenario: Two workloads claim one domain
- **WHEN** two workloads in one environment declare the same domain and path
- **THEN** validation fails and the error names both workloads

### Requirement: Environments override a closed set of fields

An environment MAY override a closed, specified set of workload and service
fields, so that a non-production environment can differ in scale and sizing
without duplicating the project. Overridable fields SHALL be enumerated in this
contract. An override of any other field SHALL be rejected, and an override
naming an undeclared workload or service SHALL be rejected.

#### Scenario: Environment reduces scale
- **WHEN** an environment overrides a workload's replica count
- **THEN** the normalized project for that environment carries the override and reports its origin as an environment override

#### Scenario: Override outside the closed set
- **WHEN** an environment overrides a field that is not overridable
- **THEN** validation fails and the error names the field and lists what may be overridden

#### Scenario: Override names an unknown workload
- **WHEN** an environment override names a workload the project does not declare
- **THEN** validation fails and the error names the unknown workload

### Requirement: Extension keys are reserved and ignored

Keys prefixed `x-` SHALL be accepted anywhere the contract accepts a mapping,
SHALL be carried through normalization unchanged, and SHALL NOT affect
validation, generation, planning, or execution. Onebox SHALL NOT assign meaning
to an `x-` key.

#### Scenario: Annotation is preserved and inert
- **WHEN** a project declares an `x-` key on a workload
- **THEN** the project validates, the key appears unchanged in the canonical form, and the generated runtime is identical to the same project without it

### Requirement: Identifiers are permanent and constrained

The application identifier SHALL name the remote layout, generated projects, and
persistent volumes, and SHALL therefore be treated as permanent: changing it
SHALL be refused against existing state rather than silently producing a second,
empty installation. Application, workload, and service identifiers SHALL be
constrained to a documented character set and length, and a documented set of
reserved names SHALL be refused.

#### Scenario: Application identifier changes against existing state
- **WHEN** a project's application identifier differs from the one recorded in the target's existing state
- **THEN** the operation is refused, names both identifiers, and no new installation is created

#### Scenario: Reserved identifier
- **WHEN** a project uses a reserved identifier for an application, workload, or service
- **THEN** validation fails and the error names the reserved word

#### Scenario: Identifier violates the character or length rule
- **WHEN** an identifier exceeds the documented length or uses characters outside the documented set
- **THEN** validation fails and names the rule it violated

### Requirement: The schema is closed and correction hints are offered

Validation SHALL reject unknown fields, unknown enum values, and values violating
declared scalar constraints. When a rejected name is close to an accepted name in
the same position, the error SHALL name the accepted alternative. Validation
SHALL report the location of each violation and SHALL NOT expose the internal
validation language.

#### Scenario: Unknown field
- **WHEN** a project declares a field the contract does not define and that is not an extension key
- **THEN** validation fails, the error names the field and its location, and no runtime is generated

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
user authors in Compose SHALL be expressible only as a workload, and SHALL NOT be
reported as managed.

#### Scenario: Workload references a Compose service
- **WHEN** a workload references a named service that exists in the referenced Compose file
- **THEN** validation succeeds and the workload's source is that Compose service

#### Scenario: Reference names a missing Compose service
- **WHEN** a workload references a service name absent from the referenced Compose file
- **THEN** validation fails and the error names the missing service and the file

#### Scenario: Service declaration attempts a Compose reference
- **WHEN** a service declaration carries a Compose reference
- **THEN** validation fails and the error states that a user-authored data service must be declared as a workload

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
versioned structured output mode. In that mode a failure SHALL emit a typed error
code, a message free of secrets, the source location where one applies, and the
command that resolves the failure. Diagnostics SHALL NOT be written to the
structured output stream.

#### Scenario: Structured validation failure
- **WHEN** validation fails and structured output is requested
- **THEN** the output carries a versioned envelope, a typed error code, a source location, and a resolving command

#### Scenario: Structured output stays machine-readable
- **WHEN** structured output is requested and local diagnostics are produced
- **THEN** diagnostics are written to the diagnostic stream and the structured stream remains parseable

### Requirement: Secrets never enter authoring output

Normalized configuration, validation errors, and published schema output SHALL
NOT contain plaintext secret values. A value sourced from a secret provider or an
environment file SHALL be represented by its reference.

#### Scenario: Secret-valued field in normalized output
- **WHEN** normalized configuration is printed for a project whose workload consumes secret values
- **THEN** the output contains the references and no resolved secret content

#### Scenario: Validation error near a secret value
- **WHEN** validation fails on a field adjacent to a secret value
- **THEN** the error text contains no secret content

### Requirement: YAML is the only authoring format

The contract SHALL accept projects authored in YAML only. A project authored in
the internal validation language SHALL be rejected with an error naming the YAML
authoring guide, so that the validation layer remains an implementation detail
rather than a second documented authoring surface.

#### Scenario: Project authored in the validation language
- **WHEN** a project is presented in the internal validation language rather than YAML
- **THEN** loading fails and the error names the YAML authoring guide
