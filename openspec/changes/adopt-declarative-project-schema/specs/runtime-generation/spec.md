## Purpose

Defines how Onebox derives an executable container runtime from a normalized
project, the remote layout and naming it owns, and the inspection and ejection
guarantees that make a generated runtime safe to depend on.

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
without silently altering any other field the user authored.

#### Scenario: Compose-referenced workload is preserved
- **WHEN** a workload sources a Compose service declaring runtime settings the contract cannot express
- **THEN** those settings appear unchanged in the generated runtime

#### Scenario: Onebox additions are visible
- **WHEN** Onebox attaches a network or routing to a Compose-referenced workload
- **THEN** the addition appears in the rendered runtime rather than being applied invisibly at execution

#### Scenario: Build-sourced workload without a resolved image
- **WHEN** a workload declares a build context and no image reference has been resolved for the release
- **THEN** generation fails before target contact and the error names the workload and the resolving command

### Requirement: The overlay onto a Compose-referenced workload is closed

Onebox SHALL overlay only an enumerated set of keys onto a Compose-referenced
workload, and that set SHALL be stated in this contract rather than left to
implementation. If the referenced service already sets an overlaid key,
generation SHALL fail and name the key and the file rather than overwriting it.

#### Scenario: Conflict on an overlaid key
- **WHEN** a referenced Compose service already sets a key Onebox overlays
- **THEN** generation fails, names the key and the file, and nothing is overwritten

#### Scenario: Keys outside the set are untouched
- **WHEN** a runtime is generated from a Compose-referenced workload
- **THEN** no key outside the enumerated overlay set is added, removed, or modified

### Requirement: The remote layout is contract, not implementation detail

Onebox SHALL own a documented directory layout on the target: a base path, a
per-application directory beneath it, versioned release directories, and a
pointer to the active release. The default base path SHALL follow the platform
convention for variable state owned by a program. The base path SHALL be
configurable in the project so that an operator can place state on a mounted
volume, and the configured value SHALL be reported in observation and bound into
plans. A documented namespace SHALL be reserved for state shared by every
application on the host, and SHALL be refused as an application identifier.

#### Scenario: Default layout
- **WHEN** no base path is configured
- **THEN** the documented default is used and reported as a default in observation

#### Scenario: Base path relocated to a mounted volume
- **WHEN** a project configures a base path
- **THEN** every release directory, journal, lock, and owned volume resolves beneath it, and the configured value is bound into the plan

#### Scenario: Reserved host namespace
- **WHEN** a project uses the reserved host namespace as its application identifier
- **THEN** validation fails and names the reserved word

### Requirement: The deploy account's required privileges are stated and checked

This contract SHALL state the privileges the configured account needs on the
target to create the layout and operate the container runtime. Onebox SHALL check
those privileges before mutating and SHALL fail with an actionable error naming
the missing privilege and the remedy, rather than failing partway through with a
permission error from an underlying command.

#### Scenario: Account cannot create the base path
- **WHEN** the configured account lacks permission to create the base path
- **THEN** the operation fails before mutation, names the path and the missing privilege, and states the remedy

#### Scenario: Account cannot operate the container runtime
- **WHEN** the configured account cannot run container commands on the target
- **THEN** the operation fails before mutation with an error naming the remedy

### Requirement: Generated resource names are derived, stable, and permanent

Generated Compose project, network, and volume names SHALL derive from declared
identifiers by a documented pattern, SHALL be stable across releases so a
rollback cannot orphan a resource, SHALL be validated against the container
runtime's length and character limits, and SHALL be truncated with a
collision-resistant suffix when a derived name would exceed them. Volume names
SHALL be treated as permanent: once a volume exists, a later release SHALL NOT
derive a different name for the same declared resource.

#### Scenario: Names are stable across releases
- **WHEN** two different releases of the same project generate a runtime
- **THEN** the project, network, and volume names are identical

#### Scenario: Derived name exceeds the runtime's limit
- **WHEN** a derived name would exceed the container runtime's length limit
- **THEN** it is truncated with a collision-resistant suffix and remains stable for the same inputs

#### Scenario: A later release would rename an existing volume
- **WHEN** a change to the naming pattern would derive a different volume name for a resource that already exists
- **THEN** it is a breaking change to this contract and cannot ship without an explicit data-migration path

### Requirement: Supporting services are generated independently of the application release

A service SHALL be generated into a project separate from the application's, with
its own persistent volumes, so that an application release, rollback, or teardown
cannot recreate or remove it.

#### Scenario: Application rollback leaves services intact
- **WHEN** an application release is rolled back
- **THEN** every supporting service's containers and volumes are untouched

#### Scenario: Application teardown leaves service volumes
- **WHEN** an application is torn down without explicitly requesting volume removal
- **THEN** supporting-service volumes still exist

### Requirement: Onebox generates the surrounding runtime

Generation SHALL produce the networks, volumes, and routing the declared
workloads require, derive routing from declared domains, ports, and protocols,
and refuse a name collision with an existing resource Onebox does not own rather
than adopting or overwriting it.

#### Scenario: Routing derived from a declared domain
- **WHEN** a workload declares a domain and a port
- **THEN** the generated runtime routes that domain to that workload's port

#### Scenario: Collision with a foreign resource
- **WHEN** a generated resource name collides with an existing resource Onebox does not own
- **THEN** generation fails, names the collision, and no existing resource is adopted or modified

### Requirement: The generated runtime is fully inspectable

Onebox SHALL render the complete generated runtime on request, without contacting
a target and without mutating state. The rendered runtime SHALL be the artifact
execution would use for the same inputs. Rendered output SHALL contain no
plaintext secret values.

#### Scenario: Rendering does not mutate
- **WHEN** a runtime is rendered
- **THEN** no target is contacted and no local or remote state changes

#### Scenario: Rendered output matches execution
- **WHEN** a runtime is rendered and then a plan is created from the same inputs
- **THEN** the runtime bound by the plan is byte-identical to the rendered runtime

#### Scenario: Secrets are absent from rendered output
- **WHEN** a runtime consuming secret values is rendered
- **THEN** the output contains references and no resolved secret content

### Requirement: Ejection transfers ownership permanently

Onebox SHALL write the generated runtime into the repository on request and, from
that point, treat those services as user-authored. Ejection SHALL refuse to
overwrite an existing file unless overwriting is explicitly requested. After
ejection Onebox SHALL NOT regenerate or silently reconcile the ejected services.

#### Scenario: Ejection writes the runtime
- **WHEN** ejection is requested and no file exists at the destination
- **THEN** the generated runtime is written and the project records those services as user-authored

#### Scenario: Ejection refuses to clobber
- **WHEN** ejection targets an existing path and overwriting was not requested
- **THEN** the operation fails, names the path, and writes nothing

#### Scenario: Ejected services are not regenerated
- **WHEN** a runtime is generated for a project whose services were previously ejected
- **THEN** the ejected services are used as authored and are not regenerated

### Requirement: Generation binds what execution will run

An executable plan SHALL bind the generated runtime by digest together with the
normalized project, the resolved image references, and the configured base path.
Execution SHALL refuse a plan whose bound runtime digest does not match the
runtime regenerated from the plan's own inputs.

#### Scenario: Runtime digest is bound
- **WHEN** a plan is created
- **THEN** the plan carries the digest of the generated runtime it will execute

#### Scenario: Regenerated runtime disagrees with the plan
- **WHEN** execution regenerates the runtime and the digest differs from the bound digest
- **THEN** execution is refused before any target mutation and the error directs the operator to re-plan

### Requirement: Generation fails closed and before target contact

Any generation failure SHALL occur before a target connection is opened, SHALL
leave no partial artifact on disk or on the target, and SHALL be reported with a
typed error code and the command that resolves it.

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
- **WHEN** a workload references a declared service Onebox does not run
- **THEN** generation succeeds and reported readiness states that the service is not managed by Onebox
