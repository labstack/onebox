## Purpose

Defines the `onebox.run/v1` authoring contract, in which the project file states
intent and Onebox derives the runtime, so that declaring an application is short,
its ownership boundary is explicit in the shape of the file, every operational
fact the project needs has a home, and the contract can grow for years without a
second redefinition.

## ADDED Requirements

### Requirement: The field model is normative and machine-checkable

The accepted shape SHALL be `schema.cue` in this change directory. It describes
the **normalised** project — shorthand expanded, documented defaults filled —
because a discriminator left to a default keeps every branch of a disjunction
alive, and a default on an optional field never materialises at all. Keys, types,
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

Four rules cannot be expressed in the schema and SHALL be enforced by the
loader: at least one environment; a non-empty `workloads` block when one is
declared; exactly one workload source, both at the top level and per workload;
and routing exclusivity, where `domain` and `port` appear together or `routes`
appears alone, never both. Cardinality validators fail against the bare
definition, and an exclusivity disjunction with an empty branch never resolves,
because a branch missing a required field is incomplete rather than invalid.
`conformance.md` records these as loader cases.

#### Scenario: Loader-enforced rule is still enforced
- **WHEN** a project declares top-level shorthand alongside a `workloads` block
- **THEN** loading fails naming both locations, even though the schema alone accepts it

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

A workload SHALL declare exactly one of `build`, `image`, or `compose`, and
exactly one role from `application`, `worker`, `daemon`, or `job`.

The `daemon` role SHALL exist for a long-running supporting container the user
still authors — a database they run themselves, a cache, a cron runner, a
scanner. It is distinguished from `application` and `worker` because environment
files SHALL NOT be projected into it and it SHALL NOT receive ingress unless it
declares a route. Without it, converting a running data service forces a choice
between calling it an application, which changes what is injected into it, and
calling it a job, which is false.

`run`, `schedule`, and `data_effect` SHALL apply only to the `job` role.
`data_effect` SHALL be required for a job because its effect on data cannot be
inferred.

A job MAY declare a `schedule`, so that a recurring task — a backup push, a
retention sweep, a nightly prune — is a first-class job that Onebox runs, rather
than requiring a third-party cron container whose runs are invisible to the
journal.

#### Scenario: Workload declares two sources
- **WHEN** a workload declares both a build context and an image reference
- **THEN** validation fails and the error states that a workload has exactly one source

#### Scenario: Job without a declared data effect
- **WHEN** a workload with the job role omits `data_effect`
- **THEN** validation fails, because the effect on data is an operator assertion and is never inferred

#### Scenario: Data service converts to a daemon
- **WHEN** a running data service is converted
- **THEN** it becomes a `daemon` workload, no environment files are projected into it, and it receives no ingress network unless it declares a route

#### Scenario: Recurring job
- **WHEN** a job declares a schedule
- **THEN** the project validates and Onebox owns the schedule, with runs recorded like any other operation

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
| Service | `resources`, `settings` |

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
entrypoint, protocol, backend scheme, and TLS mode, so that path routing, non-HTTP protocols, and TLS
behavior are expressible without a later shape change. A scalar `domain` with a
scalar `port` SHALL remain accepted as shorthand for a single HTTP route at path
`/` with TLS terminated.

#### Scenario: Scalar shorthand expands to one route
- **WHEN** a workload declares a scalar domain and port
- **THEN** the canonical form contains one route with path `/`, protocol http, and TLS terminated

#### Scenario: gRPC backend
- **WHEN** a route declares the `h2c` backend scheme
- **THEN** the project validates and the generated routing directs the proxy to speak HTTP/2 cleartext to the workload

#### Scenario: Non-HTTP route
- **WHEN** a workload declares a route with the tcp protocol
- **THEN** the project validates and the protocol is carried into the canonical form

#### Scenario: Two workloads claim one domain and path
- **WHEN** two workloads in one environment declare the same domain and path
- **THEN** validation fails and the error names both workloads

### Requirement: Prerequisites carry a condition

A workload MAY declare prerequisites. A prerequisite SHALL be a name or a name
with a condition of `started`, `healthy`, or `completed`, defaulting to
`healthy`. Converting ten real projects — five in this organization and five
unrelated open-source stacks — found that every one of them gated startup on a
dependency being healthy rather than merely started, so mere ordering is the
wrong default.

#### Scenario: Bare name defaults to healthy
- **WHEN** a workload declares a prerequisite as a bare name
- **THEN** the canonical form records the condition as `healthy`

#### Scenario: Explicit condition
- **WHEN** a workload declares a prerequisite with the `completed` condition
- **THEN** the canonical form records that condition

### Requirement: Workloads may publish host ports

A workload MAY publish a host port, for a service reached without going through
the proxy. A published port SHALL declare the host port, the container port, a bind address
defaulting to `127.0.0.1`, and a protocol of `tcp` or `udp` defaulting to `tcp`.
A workload MAY publish the same port under both protocols. Publishing on every interface SHALL
require declaring the bind address explicitly, so exposure is deliberate rather
than accidental.

#### Scenario: Default bind is loopback
- **WHEN** a workload publishes a port without a bind address
- **THEN** the canonical form binds it to `127.0.0.1`

#### Scenario: UDP port
- **WHEN** a workload publishes the same port over TCP and UDP
- **THEN** both are declared and both appear in the generated runtime

#### Scenario: Public exposure is explicit
- **WHEN** a workload publishes a port on every interface
- **THEN** the bind address is stated in the project file

### Requirement: Configuration files are staged with the release

A project MAY declare repository files to stage onto the target alongside the
release. Onebox SHALL stage each declared file with the release, preserving its
path relative to the project file, so that configuration a referenced Compose
service mounts is present when the workload starts. Each declared file SHALL be
a repository path. Without this, a Compose reference is staged but the file it
mounts is not, and the workload starts against a missing path.

#### Scenario: Mounted configuration reaches the target
- **WHEN** a project declares configuration files and a referenced service mounts one
- **THEN** the file is staged at the same relative path and the mount resolves

#### Scenario: Staged file is a repository path
- **WHEN** a declared file is absolute or escapes the repository
- **THEN** validation fails and names the path

### Requirement: Environment files are per workload, with a project-wide default

A workload MAY declare its own environment files. A project-wide list SHALL
apply to every `application` and `worker` workload that declares none, and SHALL
NOT apply to a `daemon` or to a workload with its own list.

A project-wide-only model was tried and rejected against real stacks: one project
gives a file to its web server alone, another shares one between two of four
services, and a third has three files for three services. Projecting every file
into every container would put each service's secrets in all of them.

Environment files SHALL be applied in declared order, with a later file
overriding an earlier one. Their values SHALL be available for interpolation in
referenced Compose sources and SHALL be projected into application and worker
workloads. A missing environment file SHALL fail validation. Preflight checks
SHALL assert that required keys are present and non-empty, and that named keys
exist.

#### Scenario: Workload list overrides the project list
- **WHEN** a workload declares its own environment files and the project also declares some
- **THEN** only the workload's files are projected into it

#### Scenario: Daemons receive no project-wide files
- **WHEN** a project declares environment files and a daemon declares none
- **THEN** no environment file is projected into the daemon

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

### Requirement: The declaration's boundary is stated, not discovered

The declaration SHALL cover what is common across real applications and SHALL
NOT attempt to model the whole container runtime. This contract SHALL name the
concerns it deliberately leaves to a Compose reference, so an author learns the
boundary from the documentation rather than from a rejected project.

This boundary is measured, not asserted. A survey of 276 Compose files from
popular repositories, covering 1,367 services, is recorded in `survey.md`: the
declaration expresses 66% of services and every service of 54% of projects
without a reference. The nine fields that most stood in the way were added
because each was a scalar, a bool or a flat list carrying no new concept.

Deliberately not modelled, and expressible only through a Compose reference:
device mappings, privileged execution, `shm_size`, `tmpfs` mounts, `ulimits`,
`cap_add` and `cap_drop`, `sysctls`, `security_opt`, `network_mode`, `user`, and
`init`. Each appears in real stacks — a video recorder needs devices and shared
memory, an analytics database needs file-descriptor limits — but each is rare,
runtime-specific, and would grow the contract far more than it would help.

A workload needing any of them SHALL remain fully operable: Onebox releases it,
health-gates it, routes to it, and rolls it back exactly as any other workload.

#### Scenario: Hardware access through a reference
- **WHEN** a workload requires device mappings and a shared-memory size
- **THEN** it is declared by Compose reference, and Onebox releases, health-gates, routes, and rolls it back like any other workload

#### Scenario: The boundary is documented
- **WHEN** an author looks for a concern this contract does not model
- **THEN** the authoring guide names it and directs them to the Compose reference

### Requirement: Volumes are named or bound

A workload volume SHALL be either an Onebox-managed named volume or a bind
mount declaring a source and a target. A bind-mount source SHALL be a repository
path or an absolute path on the target. Bind mounts are not an edge case: they
carry the database directory or the configuration in the majority of real
projects examined against this contract.

#### Scenario: Named volume
- **WHEN** a workload declares a volume by name
- **THEN** Onebox derives its name from the naming contract and owns its lifecycle

#### Scenario: Bind mount from the repository
- **WHEN** a workload binds a repository path to a container path
- **THEN** the source is staged with the release and mounted at the target

#### Scenario: Bind mount from the target
- **WHEN** a workload binds an absolute path on the target
- **THEN** it is mounted as declared and Onebox does not manage its contents

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

### Requirement: A declared service is run by Onebox

A service declaration SHALL cause Onebox to run that service: its image, its
durable volume, its health check, its credential, and the connection details
the application reads. Declaring `postgres: 17` SHALL be sufficient; the author
SHALL NOT have to supply a password, a port, or a data path.

The set of drivers Onebox can run SHALL be closed. A declaration naming a
driver outside it SHALL be rejected, naming the available drivers and directing
the author to a daemon workload — guessing an image from an identifier would
produce a container that starts and stores nothing durable, which is discovered
at the worst possible time.

A setting SHALL be applied through the mechanism its driver actually reads.
Where a driver has no mechanism Onebox can apply safely, the setting SHALL be
rejected rather than accepted and ignored.

The contract SHALL NOT accept a backup or restore-drill declaration. Onebox
performs neither, and a project that says `backup: {schedule: ...}` while
nothing takes a backup is the most dangerous thing this contract could contain:
it reads as protection, survives every review, and is found to be absent at the
only moment it mattered. They return when something performs them and verifies
a restore.

Where a workload or a service holds durable data and nothing copies it off the
host, diagnostics SHALL say so. Silence would read as approval.

#### Scenario: A backup declaration is refused
- **WHEN** a project declares a backup schedule
- **THEN** validation fails, because nothing would perform it

#### Scenario: Unprotected durable data is reported
- **WHEN** diagnostics run against a project with durable data
- **THEN** each durable workload and service is reported as unbacked, as a warning rather than a failure

#### Scenario: A scalar service declaration is sufficient
- **WHEN** a project declares `services: {postgres: 17}`
- **THEN** the generated runtime runs Postgres 17 with a durable volume, a health check, and a generated credential, and the project contains no password

#### Scenario: An unknown driver is refused with alternatives
- **WHEN** a service names a driver Onebox cannot run
- **THEN** validation fails, listing the drivers it can run and directing the author to declare a daemon workload instead

#### Scenario: An inapplicable setting is refused
- **WHEN** a service declares a setting its driver has no mechanism to read
- **THEN** validation fails rather than silently ignoring it

### Requirement: A service outlives every release

A service SHALL be applied outside the application's release, in its own
container-runtime project. Removing, replacing, or rolling back a release SHALL
NOT stop a service or remove its durable volume.

#### Scenario: A release teardown leaves the data
- **WHEN** the application's project is removed together with its volumes
- **THEN** every declared service is still running and its durable volume still exists

### Requirement: Values that reach a generated file are bounded

Every field whose value is written into a generated file, a unit on the target,
or a command argument SHALL be constrained to a grammar admitting only what it
legitimately means. A timezone SHALL be an IANA zone name, an image SHALL be a
registry reference, a registry server SHALL be a host with an optional port and
path, and a path SHALL contain no control character, quote, backslash, or shell
metacharacter.

The project file is not a trust boundary — whoever can edit it can already
deploy — but it is reviewed. A value that reads as a timezone while appending a
root-run command to a scheduling unit defeats the review, which is the point of
having one.

A command argument SHALL be quoted at the point of use as well as bounded at
the grammar. Either alone is one mistake away from a command.

#### Scenario: A hostile value is refused at load
- **WHEN** a timezone, image, registry server, or path contains a control character or shell metacharacter
- **THEN** validation fails, before anything is generated

#### Scenario: Ordinary values still load
- **WHEN** a project uses an IANA zone, a digest-pinned image, a registry with a port, or a nested env-file path
- **THEN** it loads unchanged

### Requirement: A workload names the connection itself

A workload declaring a prerequisite on a managed service SHALL be able to say
which of its own variables receive which part of the connection. Every
application names its own — n8n reads `DB_POSTGRESDB_HOST`, Django reads
`POSTGRES_DB` — and without this a managed service is usable only by an
application that happens to read the names Onebox chose, which almost none do.

The value SHALL come from a closed set of connection parts. A name outside it
SHALL be refused, because it would otherwise produce a variable that is
present, empty, and blamed on the application.

A part the driver does not have — a database on a cache — SHALL be omitted
rather than written empty, for the same reason.

#### Scenario: An application reads its own variable names
- **WHEN** a workload maps its variables onto a service's connection parts
- **THEN** those variables reach the container carrying the connection, and the canonical names are still available

#### Scenario: A part the driver lacks is omitted
- **WHEN** a mapping names a part the driver does not have
- **THEN** the variable is not written at all

### Requirement: A service credential is generated on the target and never travels

The credential for a managed service SHALL be generated on the target, exactly
once, and SHALL NOT appear in the project, the generated runtime, or its digest.
Onebox SHALL write the connection details a workload needs — a URL and its
parts — to a target-side file that only workloads declaring a prerequisite on
that service read.

An established credential SHALL NOT be regenerated: rotating it silently would
leave the application holding a credential its service has forgotten. Every
file derived from it SHALL be rewritten on each apply, so adding a workload or
renaming a variable takes effect without the credential moving.

#### Scenario: No credential is present in anything that travels
- **WHEN** a runtime is generated for a project declaring a managed service
- **THEN** the runtime contains no credential, and the digest does not change when the credential on a target does

#### Scenario: A re-apply preserves the credential
- **WHEN** services are applied again on a target that already has them
- **THEN** the existing credential is left unchanged

### Requirement: A scheduled job runs on the host's own scheduler

A job declaring a schedule SHALL be installed as a timer on the target, so it
runs without any Onebox process being alive and survives a reboot. The
translation from the declared cron expression to the host's calendar
expression SHALL be exact: a form whose meaning cannot be preserved SHALL be
refused at load, naming the reason, rather than approximated into a schedule
nobody asked for.

A cron expression setting both a day of month and a day of week SHALL be
refused, because cron runs the job when either matches and a calendar
expression cannot express that.

A timer SHALL invoke the job through the current release, so a scheduled job
runs the code that is live and a rollback moves it back with everything else. A
job that is no longer scheduled SHALL have its timer removed; one left behind
would keep running with nothing in the project to explain it.

The host SHALL validate the derived expression before any timer is installed.

#### Scenario: A declared schedule runs
- **WHEN** a job declares a cron schedule and the project is deployed
- **THEN** a timer exists on the target that invokes that job through the current release

#### Scenario: An untranslatable schedule is refused
- **WHEN** a job declares a cron expression whose meaning cannot be preserved exactly
- **THEN** validation fails, naming the expression and the reason

#### Scenario: An undeclared schedule is removed
- **WHEN** a job that previously had a schedule no longer declares one
- **THEN** its timer is removed from the target

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
