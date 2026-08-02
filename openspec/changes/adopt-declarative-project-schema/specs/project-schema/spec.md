## Purpose

Defines the `onebox.run/v1` authoring contract, in which the project file states
intent and Onebox derives the runtime, so that declaring an application is short,
its ownership boundary is explicit in the shape of the file, every operational
fact the project needs has a home, and the contract can grow for years without a
second redefinition.

## ADDED Requirements

### Requirement: The field model is normative and machine-checkable

The accepted shape SHALL be `schema.cue` in this change directory. Keys, types,
requiredness, exclusivity, bounds, enumerations, defaults, and closure are
normative there. Prose in this specification SHALL NOT contradict it; where they
disagree, the schema governs.

The shape is stated as a compiled artifact rather than prose because two review
rounds established that prose cannot carry requiredness, exclusivity, and bounds
precisely enough for two implementations to accept the same corpus. `schema.cue`
compiles and is exercised by the corpus in `conformance.md`, which SHALL be the
acceptance test.

A minimum project, for illustration only:

```yaml
api_version: onebox.run/v1
app: ledger
environments:
  production:
    server: root@1.2.3.4
build: .
port: 8080
health: /healthz
services:
  postgres: 18
```

#### Scenario: Two implementations agree
- **WHEN** two implementations are given the conformance corpus
- **THEN** they accept and reject the same projects and produce the same canonical form

#### Scenario: Schema and prose disagree
- **WHEN** a prose statement in this specification conflicts with the schema
- **THEN** the schema governs and the prose is a defect to be corrected

#### Scenario: Default is applied and attributed
- **WHEN** a project omits a field carrying a default
- **THEN** the canonical form contains the documented default and reports its origin as a default

### Requirement: Path kinds are distinguished

The contract SHALL distinguish three kinds of path, because a single rule cannot
govern them. A **repository path** — Compose references, environment files,
preflight files, proxy configuration, secret files, build contexts, the ejection
destination — SHALL resolve relative to the directory containing the project
file regardless of the working directory, and SHALL be refused if it is absolute
or resolves outside the repository root, including through a symbolic link. A
**target path** — the base path, a volume's mount point — SHALL be absolute and
is not subject to repository containment. A **request path** — a route's path, a
health check's HTTP path, a verification path — SHALL begin with `/` and denotes
a location in a URL, not on any filesystem.

#### Scenario: Working directory does not affect resolution
- **WHEN** the same project is loaded from two different working directories
- **THEN** every repository path resolves identically and the canonical forms match

#### Scenario: Repository path escapes the repository
- **WHEN** a repository path resolves outside the repository root, directly or through a symbolic link
- **THEN** validation fails and names the path

#### Scenario: Absolute repository path
- **WHEN** an environment file is declared as an absolute path
- **THEN** validation fails, because environment files are repository paths

#### Scenario: Target path is absolute
- **WHEN** a base path is declared
- **THEN** it is required to be absolute and is not subject to repository containment

### Requirement: Every fact the classifier contract expressed has a home

Restructuring SHALL remove no shipped capability. Each fact below SHALL be
expressible, with the stated home:

| Previous fact | New home |
|---|---|
| `components.<>.type` application, worker, job | `workloads.<>.role` |
| `components.<>.type` postgres, mysql, redis | `services.<>` for new declarations; **existing installations convert to `workloads.<>`** — see below |
| `components.<>.type` service | `workloads.<>` sourced by image |
| `components.<>.service` | `workloads.<>.compose`, or the generated service |
| `components.<>.command` (string or `{run, local}`) | `workloads.<>.command`, same union |
| `components.<>.data_effect` including `unknown` | `workloads.<>.data_effect`, same members |
| `deployment.strategy`, `replicas` | `workloads.<>.strategy`, `.replicas` |
| `readiness`, `drain` | `workloads.<>.health`, `.drain` |
| `persistence.mode` including `external`, and volume identities | `workloads.<>.persistence`, `.volumes` |
| `components.<>.protection` on a data service | `services.<>.backup`, with independent backup and restore-drill schedules |
| `components.<>.protection` on an application, worker, or generic service | `workloads.<>.protection` |
| `policy.*` including `allow_agent_proposals`, `minimum_plan_schema` | `environments.<>.policy.*` |
| `verification` http, exec, url, `contains`, `advisory` | `verification[]` |
| `hooks.<phase>.local` | `hooks.<phase>.local` |
| `proxy.kind` including `none`, `managed`, `image`, `config`, `network` | `proxy.*` |
| `notifications.webhook`, `on`, `format` | `notifications.<name>.*` |
| `registry.server`, `username`, `password_env` | `registries.<name>.*` |
| `observability.logs`, `metrics`, `alerts` | `observability.*` |
| `runtime.env_files`, `preflight` | `runtime.*` |
| `deployment.order`, `retain_releases` | `deployment.*` |
| `migration_policy` including `expand-only` | `deployment.migration_policy`, same members |

Because a service declaration is inert in this change, a data service that is
already running SHALL convert to a workload sourced by its existing Compose
service, preserving exactly what runs. `services.<>` is the destination once a
driver exists to run it; converting a live database to an inert declaration
would delete it, so this contract SHALL NOT do that.

#### Scenario: Running data service converts without loss
- **WHEN** a project already running a data service is converted
- **THEN** it becomes a workload sourced by its existing Compose service and the generated runtime still runs it

#### Scenario: Every existing project is expressible
- **WHEN** each existing project in this organization is expressed under this contract
- **THEN** every operational fact it declared has a home, and any that does not is a defect in this contract

#### Scenario: Proxy can be disabled
- **WHEN** a project declares that no proxy is managed
- **THEN** the project validates and no proxy is generated or required

#### Scenario: Advisory verification does not fail a release
- **WHEN** an advisory verification step fails
- **THEN** the release is not failed by it and the result is reported

### Requirement: Projects declare a supported schema identity

A project SHALL declare `api_version: onebox.run/v1`. Onebox SHALL reject an
absent, malformed, or unsupported identity before validation, target contact, or
generation, naming both the declared identity and the supported identities.

#### Scenario: Unsupported schema identity
- **WHEN** a project declares an unsupported identity
- **THEN** loading fails, no target connection is attempted, and the error names the declared and supported identities

#### Scenario: Project written against the withdrawn classifier contract
- **WHEN** a project declares `onebox.run/v1` but uses the withdrawn `components` block
- **THEN** loading fails with an error naming the block and the authoring guide, not a generic unknown-field error

### Requirement: A minimum project is an identifier, a server, and one workload source

Onebox SHALL accept a project declaring only an application identifier, a server,
and one workload source. Top-level `build`, `image`, `compose`, `port`, `health`,
and `domain` SHALL be shorthand for a single workload named for the application.

#### Scenario: Minimum project validates
- **WHEN** a project declares an application identifier, a server, and a single build context
- **THEN** validation succeeds and the canonical form contains one workload named for the application

#### Scenario: No workload source
- **WHEN** no workload declares a source
- **THEN** validation fails and the error states that at least one workload source is required

#### Scenario: Shorthand and explicit form conflict
- **WHEN** a project declares top-level workload fields and also declares a `workloads` block
- **THEN** validation fails and the error names both locations

### Requirement: A workload declares exactly one source and one role

A workload SHALL declare exactly one of `build`, `image`, or `compose`. A
container that is neither built by the user nor backed by a driver SHALL be
expressible as a workload by image reference. `run` and `data_effect` SHALL apply
only to the `job` role, and `data_effect` SHALL be required for a job because its
effect on data cannot be inferred.

#### Scenario: Workload declares two sources
- **WHEN** a workload declares both a build context and an image reference
- **THEN** validation fails and the error states that a workload has exactly one source

#### Scenario: Job without a declared data effect
- **WHEN** a workload with the job role omits `data_effect`
- **THEN** validation fails, because the effect on data is an operator assertion and is never inferred

#### Scenario: Job field on a non-job workload
- **WHEN** an application or worker declares `run` or `data_effect`
- **THEN** validation fails naming the field and the role

### Requirement: The contract grows additively under a stable identity

A later release MAY add optional fields, additional enum members, and object
forms of existing scalars. It SHALL NOT remove a field, narrow an accepted value,
or change the meaning of an accepted project. A scalar form, once accepted, SHALL
remain accepted permanently.

Adding an object form to an existing scalar is therefore additive, not breaking.
It is nonetheless preferred to design the object form before release, because
adding one later changes the canonical form and therefore the generated runtime
of projects that did not change.

Because an older binary cannot represent a newer field, an environment MAY
declare a minimum runner version, and an older binary SHALL fail closed rather
than silently ignore what it cannot represent.

#### Scenario: Later release adds an optional field
- **WHEN** a later release adds an optional field and a project omits it
- **THEN** the project's meaning is unchanged and it validates as before

#### Scenario: Older runner meets a project that requires a newer one
- **WHEN** a project declares a minimum runner version newer than the running binary
- **THEN** validation fails closed and names the required version

#### Scenario: Narrowing is refused
- **WHEN** a change would cause a previously accepted project to be rejected or to mean something different
- **THEN** it is a breaking change and cannot ship under this identity

### Requirement: Environment overrides are closed, merge predictably, and win

An environment MAY override only these fields:

| Target | Overridable |
|---|---|
| Workload | `replicas`, `resources`, `env`, `strategy`, `routes` |
| Service | `resources`, `settings`, `backup` |

A field is overridable only if changing it cannot change which artifact runs or
what it does to data. `build`, `image`, `compose`, `command`, `run`,
`data_effect`, `volumes`, `persistence`, `driver`, and `version` SHALL therefore
be refused, as SHALL any field not listed.

Merge semantics SHALL be: a scalar or list replaces wholesale; a mapping merges
key by key, and a null value removes a key. An override naming an undeclared
workload or service SHALL be rejected, and an override SHALL NOT introduce one.

An environment override SHALL take precedence over the project-level value.

#### Scenario: Override beats an explicit project value
- **WHEN** a project declares three replicas and an environment overrides it to one
- **THEN** the canonical form for that environment carries one and reports its origin as an environment override

#### Scenario: Mapping override merges
- **WHEN** an environment overrides one key of a workload's environment mapping
- **THEN** the other keys are retained and only that key changes

#### Scenario: Null removes a key
- **WHEN** an environment override sets a mapping key to null
- **THEN** the key is absent from the canonical form for that environment

#### Scenario: List override replaces
- **WHEN** an environment overrides a workload's routes
- **THEN** the declared list replaces the project's list rather than appending to it

#### Scenario: Override outside the closed set
- **WHEN** an environment overrides a field that is not overridable
- **THEN** validation fails and the error names the field and lists what may be overridden

#### Scenario: Override introduces a workload
- **WHEN** an override declares a workload the project does not declare
- **THEN** validation fails and the error names it

### Requirement: Value precedence is defined and every value carries its origin

Resolution SHALL apply, highest first: an environment override; an explicit
project value; a value expanded from shorthand; a value derived from another
declared field; the documented contract default. Every field in the canonical
form SHALL carry its origin.

#### Scenario: Origin is reported for every field
- **WHEN** the canonical form is printed
- **THEN** every field reports one of override, explicit, shorthand, derived, or default

### Requirement: Routes are first-class

Routing SHALL be declared as a list of route objects carrying domain, path, port,
protocol, and TLS mode, so that path routing, non-HTTP protocols, and TLS
behavior are expressible without a later shape change. A scalar `domain` with a
scalar `port` SHALL remain accepted as shorthand for a single HTTP route at path
`/` with TLS terminated.

#### Scenario: Scalar shorthand expands to one route
- **WHEN** a workload declares a scalar domain and port
- **THEN** the canonical form contains one route with path `/`, protocol http, and TLS terminated

#### Scenario: Non-HTTP route
- **WHEN** a workload declares a route with the tcp protocol
- **THEN** the project validates and the protocol is carried into the canonical form

#### Scenario: Two workloads claim one domain and path
- **WHEN** two workloads in one environment declare the same domain and path
- **THEN** validation fails and the error names both workloads

### Requirement: Environment file semantics are defined

Environment files SHALL be applied in declared order, with a later file
overriding an earlier one. Their values SHALL be available for interpolation in
referenced Compose sources and SHALL be projected into application and worker
workloads. A missing environment file SHALL fail validation. Preflight checks
SHALL assert that required keys are present and non-empty, and that named keys
exist.

#### Scenario: Later file wins
- **WHEN** two environment files declare the same key
- **THEN** the value from the later file is used

#### Scenario: Missing environment file
- **WHEN** a declared environment file does not exist
- **THEN** validation fails and names the file

#### Scenario: Required key absent
- **WHEN** a preflight check requires a key that is absent or empty
- **THEN** validation fails and names the key and the file

### Requirement: Identifiers are constrained, reserved, and permanent

Application, workload, and service identifiers SHALL match
`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`, so a single-letter identifier is legal. Underscore is excluded so that it can join
derived names injectively. An application identifier SHALL NOT begin `ob-`, which
is reserved for generated resources, and SHALL NOT be `_host`, `ob`, or `proxy`.

The application identifier names the layout, projects, and volumes and SHALL be
permanent: a declared identifier disagreeing with the one recorded on the target
SHALL be refused rather than silently producing a second, empty installation.

#### Scenario: Single-character identifier
- **WHEN** an identifier is a single letter
- **THEN** validation succeeds

#### Scenario: Reserved prefix
- **WHEN** an application identifier begins `ob-`
- **THEN** validation fails and states that the prefix is reserved for generated resources

#### Scenario: Underscore in an identifier
- **WHEN** an identifier contains an underscore
- **THEN** validation fails, because underscore joins derived names

#### Scenario: Application identifier changes against existing state
- **WHEN** the declared application identifier differs from the one recorded on the target
- **THEN** the operation is refused, names both, and no new installation is created

### Requirement: Extension keys are reserved and inert

Keys prefixed `x-` SHALL be accepted wherever the contract accepts a mapping,
carried through normalization unchanged, and SHALL NOT affect validation,
generation, planning, or execution.

#### Scenario: Annotation is preserved and inert
- **WHEN** a project declares an `x-` key on a workload
- **THEN** it validates, the key appears unchanged in the canonical form, and the generated runtime is identical to the same project without it

### Requirement: The schema is closed and correction hints are offered

Validation SHALL reject unknown fields, unknown enum values, and values violating
declared constraints, reporting every violation with its source location, and
SHALL NOT expose the internal validation language. A rejected name close to an
accepted name in the same position SHALL produce a hint naming the alternative.

#### Scenario: Unknown field
- **WHEN** a project declares an undefined field that is not an extension key
- **THEN** validation fails naming the field and its location, and no runtime is generated

#### Scenario: Near-miss field name
- **WHEN** a rejected field name is close to an accepted one valid in that position
- **THEN** the error names the accepted alternative

#### Scenario: Multiple violations
- **WHEN** a project contains more than one violation
- **THEN** every violation is reported rather than stopping at the first

### Requirement: Raw Compose is bounded to workloads

A workload MAY reference a named service in a user-authored Compose file. A
service declaration SHALL NOT accept a Compose reference or raw runtime
override. A user-authored data service SHALL be expressible only as a workload,
and SHALL NOT be reported as managed.

#### Scenario: Reference names a missing Compose service
- **WHEN** a workload references a service absent from the referenced file
- **THEN** validation fails naming the service and the file

#### Scenario: Service declaration attempts a Compose reference
- **WHEN** a service declaration carries a Compose reference
- **THEN** validation fails, stating that a user-authored data service must be a workload

### Requirement: Service declarations are inert in this contract

A service declaration SHALL validate and normalize, and SHALL NOT cause Onebox to
create, converge, tune, back up, or upgrade that service. Observation SHALL
report it as declared and not managed until a driver capability establishes
otherwise.

#### Scenario: Declared service is honestly reported
- **WHEN** observation reports a declared service
- **THEN** it is reported as declared and not managed, and no backup or restore guarantee is implied

### Requirement: A machine-readable schema is published for editors

Each release SHALL publish a JSON Schema derived from the same source that
validates projects, so the two cannot diverge within a release. It SHALL be
embedded in the binary and writable to a repository path on request.
Scaffolding SHALL write a reference to it into the project file as a
`yaml-language-server` comment on the first line.

#### Scenario: Published schema matches the enforced contract
- **WHEN** the published schema is compared against the contract that release enforces
- **THEN** they accept and reject the same projects

#### Scenario: Scaffolded project references the schema
- **WHEN** a project is scaffolded
- **THEN** its first line is a schema reference comment resolving to that release's schema

### Requirement: Authoring surfaces are agent-operable

Commands that validate a project or print its configuration SHALL support a
versioned structured output mode carrying a schema identity, and on failure a
typed error code drawn from an enumerated set, a secret-free message, a source
location where one applies, and the command that resolves it. Diagnostics SHALL
NOT be written to the structured stream.

#### Scenario: Structured validation failure
- **WHEN** validation fails and structured output is requested
- **THEN** the output carries a schema identity, an enumerated error code, a source location, and a resolving command

#### Scenario: No untyped failure
- **WHEN** any failure path in this contract is exercised
- **THEN** the emitted error code belongs to the enumerated set

### Requirement: Secrets never enter authoring output

Normalized configuration, validation errors, and schema output SHALL NOT contain
plaintext secret values; a value sourced from a secret provider or environment
file SHALL be represented by its reference.

#### Scenario: Secret-valued field in normalized output
- **WHEN** normalized configuration is printed for a project consuming secret values
- **THEN** the output contains references and no resolved secret content

### Requirement: YAML is the only authoring format

The contract SHALL accept YAML only. A project authored in the internal
validation language SHALL be rejected with an error naming the YAML authoring
guide.

#### Scenario: Project authored in the validation language
- **WHEN** a project is presented in the internal validation language
- **THEN** loading fails and the error names the YAML authoring guide
