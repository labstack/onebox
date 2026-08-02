## Purpose

Defines how Onebox derives an executable container runtime from a normalized v2
project, so that users state intent instead of maintaining infrastructure, and
defines the inspection and ejection guarantees that make a generated runtime safe
to depend on.

## ADDED Requirements

### Requirement: Generation is deterministic and content-addressable

Generating a runtime from the same normalized project, the same referenced
Compose sources, and the same resolved image references SHALL produce a
byte-identical runtime and an identical content digest. Generation SHALL NOT
depend on wall-clock time, map iteration order, environment variables that are
not declared inputs, or the host on which it runs.

#### Scenario: Repeated generation is identical
- **WHEN** a runtime is generated twice from identical inputs
- **THEN** both runtimes are byte-identical and their digests match

#### Scenario: Changed input changes the digest
- **WHEN** any declared input changes
- **THEN** the generated runtime's digest differs from the previous digest

### Requirement: Workloads render according to their declared source

A workload declared with an image reference SHALL render that reference. A
workload declared with a build context SHALL render the release's resolved image
reference for that context. A workload declared with a Compose reference SHALL
render the referenced service, and Onebox SHALL apply only the additions it owns
— network attachment, release identity, and proxy routing — without silently
altering any other field the user authored.

#### Scenario: Compose-referenced workload is preserved
- **WHEN** a workload sources a Compose service that declares runtime settings the contract cannot express
- **THEN** those settings appear unchanged in the generated runtime

#### Scenario: Onebox additions are visible
- **WHEN** Onebox attaches a network or routing to a Compose-referenced workload
- **THEN** the addition appears in the rendered runtime rather than being applied invisibly at execution

#### Scenario: Build-sourced workload without a resolved image
- **WHEN** a workload declares a build context and no image reference has been resolved for the release
- **THEN** generation fails before target contact and the error names the workload and the resolving command

### Requirement: Onebox generates the surrounding runtime

Generation SHALL produce the networks, volumes, and proxy routing the declared
workloads require, derive routing from declared domains and ports, and place
generated resources under a naming scheme that is stable across releases and
collision-checked before use. A naming collision with an existing resource
Onebox does not own SHALL fail generation rather than adopt or overwrite it.

#### Scenario: Routing derived from a declared domain
- **WHEN** a workload declares a domain and a port
- **THEN** the generated runtime routes that domain to that workload's port

#### Scenario: Collision with a foreign resource
- **WHEN** a generated resource name collides with an existing resource Onebox does not own
- **THEN** generation fails, names the collision, and no existing resource is adopted or modified

### Requirement: The generated runtime is fully inspectable

Onebox SHALL render the complete generated runtime on request, without contacting
a target and without mutating any state. The rendered runtime SHALL be the same
artifact execution would use for the same inputs. Rendered output SHALL contain
no plaintext secret values; a value sourced from a secret provider or environment
file SHALL appear as its reference.

#### Scenario: Rendering does not mutate
- **WHEN** a runtime is rendered
- **THEN** no target is contacted and no local or remote state changes

#### Scenario: Rendered output matches execution
- **WHEN** a runtime is rendered and then a plan is created from the same inputs
- **THEN** the runtime bound by the plan is byte-identical to the rendered runtime

#### Scenario: Secrets are absent from rendered output
- **WHEN** a runtime that consumes secret values is rendered
- **THEN** the output contains references and no resolved secret content

### Requirement: Ejection transfers ownership permanently

Onebox SHALL write the generated runtime into the repository on request and, from
that point, treat those services as user-authored. Ejection SHALL refuse to
overwrite an existing file unless overwriting is explicitly requested. After
ejection Onebox SHALL NOT regenerate or silently reconcile the ejected services,
and the project SHALL state that they are user-authored.

#### Scenario: Ejection writes the runtime
- **WHEN** ejection is requested for a project with no existing runtime file at the destination
- **THEN** the generated runtime is written and the project records those services as user-authored

#### Scenario: Ejection refuses to clobber
- **WHEN** ejection targets a path that already exists and overwriting was not requested
- **THEN** the operation fails, names the path, and writes nothing

#### Scenario: Ejected services are not regenerated
- **WHEN** a runtime is generated for a project whose services were previously ejected
- **THEN** the ejected services are used as authored and are not regenerated

### Requirement: Generation binds what execution will run

An executable plan SHALL bind the generated runtime by digest together with the
normalized project and the resolved image references. Execution SHALL refuse a
plan whose bound runtime digest does not match the runtime regenerated from the
plan's own inputs.

#### Scenario: Runtime digest is bound
- **WHEN** a plan is created
- **THEN** the plan carries the digest of the generated runtime it will execute

#### Scenario: Regenerated runtime disagrees with the plan
- **WHEN** execution regenerates the runtime from a plan's inputs and the digest differs from the bound digest
- **THEN** execution is refused before any target mutation and the error directs the operator to re-plan

### Requirement: Generation fails closed and before target contact

Any generation failure — unresolvable source, missing Compose reference, naming
collision, unsatisfiable routing — SHALL occur before a target connection is
opened, SHALL leave no partial artifact on disk or on the target, and SHALL be
reported with a typed error code and the command that resolves it.

#### Scenario: Failure leaves no partial artifact
- **WHEN** generation fails partway through
- **THEN** no partial runtime is written locally or staged remotely

#### Scenario: Failure precedes connection
- **WHEN** a project cannot generate a runtime
- **THEN** no target connection is attempted

### Requirement: Declared services produce no runtime

Generation SHALL NOT emit containers, volumes, or networks for a service
declaration. Until a driver capability exists, a service declaration SHALL affect
only validation, normalization, and honest reporting.

#### Scenario: Service declaration is inert during generation
- **WHEN** a runtime is generated for a project declaring a service
- **THEN** the generated runtime contains nothing for that service

#### Scenario: Workload depending on a declared service
- **WHEN** a workload references a declared service that Onebox does not run
- **THEN** generation succeeds and the reported readiness states that the service is not managed by Onebox
