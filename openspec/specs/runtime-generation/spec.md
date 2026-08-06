# runtime-generation Specification

## Purpose
Defines how Onebox derives an executable container runtime from a normalized
project, the remote layout and generated names it owns, and the inspection and
ejection guarantees that make a generated runtime safe to depend on.
## Requirements
### Requirement: Generation is local, pure, and content-addressable

Generating a runtime SHALL be a local operation that opens no target connection.
It SHALL be a pure function of its runtime-affecting inputs: the normalized
project, the referenced Compose sources, and the resolved image references.
Identical runtime-affecting inputs SHALL produce a byte-identical runtime and an
identical digest. Generation SHALL NOT depend on wall-clock time, map iteration
order, undeclared environment variables, or the host on which it runs.

A change to an input that does not affect the runtime — an extension key, a
comment — SHALL leave the generated runtime and its digest unchanged. A service
declaration is runtime-affecting: its version binds into the digest, so a
database upgrade under an untouched application cannot pass unnoticed.

Every `$` in a value Onebox generates SHALL be escaped for the container
runtime's own interpolation. The runtime file is read by Compose, which
substitutes `$VAR` from the host environment; an unescaped generated `$` would
silently become the empty string, which is how a cache ends up running with an
empty password while the application holds a real one. Content copied verbatim
from a Compose file the author referenced SHALL NOT be escaped — interpolation
is that file's own contract.

#### Scenario: Repeated generation is identical
- **WHEN** a runtime is generated twice from identical inputs
- **THEN** both runtimes are byte-identical and their digests match

#### Scenario: Runtime-affecting change alters the digest
- **WHEN** a runtime-affecting input changes
- **THEN** the generated runtime's digest differs from the previous digest

#### Scenario: Non-runtime-affecting change does not alter the digest
- **WHEN** only an extension key or an inert service declaration changes
- **THEN** the generated runtime is byte-identical and its digest is unchanged

#### Scenario: Generation opens no connection
- **WHEN** a runtime is generated
- **THEN** no target connection is attempted, whether generation succeeds or fails

### Requirement: Workloads render according to their declared source

A workload declared with an image reference SHALL render that reference. A
workload declared with a build context SHALL render the release's resolved image
reference for that context. A workload declared with a Compose reference SHALL
render the referenced service with only the overlay below applied.

#### Scenario: Compose-referenced workload is preserved
- **WHEN** a workload sources a Compose service declaring runtime settings the contract cannot express
- **THEN** those settings appear unchanged in the generated runtime

#### Scenario: Build-sourced workload without a resolved image
- **WHEN** a workload declares a build context and no image reference has been resolved for the release
- **THEN** generation fails and the error names the workload and the resolving command

### Requirement: The overlay onto a Compose-referenced workload is exact

Onebox SHALL add exactly the following to a Compose-referenced workload, and
SHALL modify nothing else:

| Addition | Exact content |
|---|---|
| Ingress network | append the project's `proxy.network`, resolved for the environment, to the service's networks, preserving existing entries in order |
| Identity labels | `ob.app`, `ob.release`, `ob.workload` |
| Routing labels | `traefik.enable`, `traefik.docker.network`, and per route: `traefik.<protocol>.routers.<router>.rule`, `.entrypoints`, `.tls`, and `traefik.<protocol>.services.<service>.loadbalancer.server.port`, using the router and proxy service names from the naming table |
| Environment projection | append the workload's resolved `env_files` entries, as staged paths, then its managed-service connection files, to the service's `env_file` list, preserving referenced entries first and in order; create the key when absent |

The ingress network SHALL be the project's `proxy.network` as resolved for the
selected environment, not a fixed name. When `proxy.kind` is `none` or `proxy.managed` is false, Onebox SHALL
add neither routing labels nor a network, and a workload declaring a route under
that configuration SHALL fail validation naming the conflict. Routing labels SHALL
otherwise be added only when the workload declares at least one route.

The overlay SHALL be refused, naming the key and the file, when the referenced
service already attaches the ingress network, already declares a label in the
`ob.` namespace, or — when the workload declares a route — already declares a
label in the `traefik.` namespace.

A referenced file SHALL be read as plain YAML rather than through a Compose
loader. Loading would interpolate variables, follow `extends` and `include`, and
validate the whole file, so referencing one service would fail on an unrelated
service's missing variable. Referencing one service SHALL depend only on that
service.

`container_name` SHALL be refused unconditionally, and SHALL NOT be silently
removed. Onebox owns container naming, because container names are host-global
and an authored name reintroduces exactly the cross-application collision the
naming contract exists to prevent. An earlier draft permitted a fixed name on a
single-replica recreate workload; that exception contradicted the naming
contract, since a preserved name such as `feed` is not application-scoped.
Conversion removes the key, which is a one-line edit.

A referenced service declaring `network_mode` SHALL be refused **when a network
would be attached**, because the container runtime rejects a service carrying
both `network_mode` and `networks`. When the proxy is disabled no network is
attached, so `network_mode` SHALL be preserved.

The projection row exists because a workload's environment follows from its
role, and a workload adopted from a Compose file has a role like any other.
Without it the overlay silently withheld the project's environment from every
`compose:`-sourced workload, whatever its role — behaviour nothing stated and
nobody chose. "SHALL modify nothing else" continues to hold: the enumeration is
larger, not weaker.

The overlay SHALL additionally be refused, naming the variable, the service and
the file, when the referenced service's `environment` sets a variable a
managed-service connection supplies to the workload. The container runtime
places `environment` above `env_file`, so such a key would outrank the
connection, and a credential generated on the target exists nowhere else.

Ejection SHALL strip everything the overlay adds, which now includes the
projected `env_file` entries, so the ejected file remains ordinary
user-authored Compose and generating from it again does not duplicate them.

#### Scenario: Projection appends and preserves
- **WHEN** a referenced service already declares an `env_file` list and the workload resolves two entries
- **THEN** the generated service's `env_file` carries the referenced entries first, then the resolved entries in order, then any connection files, and no other key changes

#### Scenario: A referenced environment key claiming a connection variable is refused
- **WHEN** a compose-sourced workload needs a managed service and the referenced service's `environment` sets a variable that connection supplies
- **THEN** generation fails naming the variable, the service and the file

#### Scenario: Ejection strips the projection
- **WHEN** a workload whose runtime carries projected `env_file` entries is ejected and then generated again
- **THEN** the ejected file carries only what the author's declaration implies, and generation does not duplicate the entries

#### Scenario: Fixed container name is refused
- **WHEN** a Compose-referenced workload sets `container_name`
- **THEN** generation fails naming the key and the file, and the key is not removed

#### Scenario: network_mode with the proxy enabled
- **WHEN** a referenced Compose service declares `network_mode` and a network would be attached
- **THEN** generation fails naming the key

#### Scenario: network_mode with the proxy disabled
- **WHEN** a referenced Compose service declares `network_mode` and the proxy is disabled
- **THEN** generation succeeds and the key is preserved

#### Scenario: Proxy disabled
- **WHEN** the environment disables the proxy and a workload declares a route
- **THEN** validation fails naming the conflict, and no routing label or network is added

#### Scenario: Existing routing labels conflict
- **WHEN** a Compose-referenced workload declares a route and the referenced service already sets a `traefik.` label
- **THEN** generation fails naming the label and the file

#### Scenario: Existing networks are preserved
- **WHEN** a Compose-referenced workload already attaches its own networks
- **THEN** the generated runtime retains them in order with the configured ingress network appended

#### Scenario: Keys outside the overlay are untouched
- **WHEN** a runtime is generated from a Compose-referenced workload
- **THEN** no key outside the overlay is added, removed, or modified

### Requirement: Target preflight is a separate phase from generation

Checks that require the target — resource-name collisions, ownership, and account
privileges — SHALL run in a target preflight phase after generation and before
any mutation. Preflight SHALL open a connection, SHALL make no change, and SHALL
fail before the first mutating command. Generation and preflight SHALL report
failures distinguishably, so a caller can tell a project defect from a target
condition.

#### Scenario: Collision detected during preflight
- **WHEN** a generated resource name collides with an existing resource Onebox does not own, determined by its labels
- **THEN** preflight fails, names the collision, and no existing resource is adopted, modified, or removed

#### Scenario: Preflight makes no change
- **WHEN** preflight fails for any reason
- **THEN** no target state has been modified

#### Scenario: Failure phases are distinguishable
- **WHEN** an operation fails
- **THEN** the reported failure identifies whether it arose during local generation or target preflight

### Requirement: The deploy account's required privileges are stated and checked

This contract SHALL state the privileges the configured account needs to create
the layout and operate the container runtime. Preflight SHALL check them and
fail with the missing privilege and its remedy, rather than failing partway
through with an error from an underlying command.

#### Scenario: Account cannot create the base path
- **WHEN** the configured account lacks permission to create the base path
- **THEN** preflight fails, names the path and the missing privilege, and states the remedy

#### Scenario: Account cannot operate the container runtime
- **WHEN** the configured account cannot run container commands on the target
- **THEN** preflight fails with an error naming the remedy

### Requirement: The remote layout is contract

Onebox SHALL own a directory layout on the target laid out by the naming patterns
below. The default base path SHALL be `/var/lib/ob`, which is what the Filesystem
Hierarchy Standard prescribes for variable state owned by a program that installs
nothing of its own.

The base path SHALL be configurable per environment, because different
environments commonly place state on different mounted volumes, with a
project-level value serving as the default for environments that do not set one.
The resolved value SHALL be reported in observation and bound into the plan.

The `_host` namespace beneath the base path SHALL be reserved for state shared by
every application on the host.

#### Scenario: Default base path
- **WHEN** neither the project nor the environment configures a base path
- **THEN** `/var/lib/ob` is used and reported as a default in observation

#### Scenario: Environment relocates state
- **WHEN** an environment configures a base path
- **THEN** every release directory, journal, lock, and owned volume for that environment resolves beneath it, and the resolved value is bound into the plan

### Requirement: Generated names are injective, stable, and permanent

Derivation SHALL be injective: no two distinct inputs may produce the same name.
Because identifiers may contain hyphens, hyphen SHALL NOT join them — a pattern
`ob-<app>-<service>` maps both (`a-b`, `c`) and (`a`, `b-c`) to `ob-a-b-c`.
Underscore is excluded from the identifier grammar and accepted by the container
runtime in project and volume names, so underscore SHALL join every derived name.

| Resource | Pattern | Example |
|---|---|---|
| Application Compose project | `<app>` | `ledger` |
| Service Compose project | `ob_<app>_<service>` | `ob_ledger_postgres` |
| Service volume | `ob_<app>_<service>_<volume>` | `ob_ledger_postgres_data` |
| Workload volume | `ob_<app>_<workload>_<volume>` | `ob_ledger_web_uploads` |
| Router | `<app>_<workload>_r<index>` | `ledger_web_r0` |
| Proxy service, first route | `<app>_<workload>` | `ledger_web` |
| Proxy service, later routes | `<app>_<workload>_r<index>` | `ledger_web_r1` |
| Shared ingress network | the environment's `proxy.network` | `ob-ingress` |
| Proxy Compose project | `ob-proxy` | `ob-proxy` |
| Application directory | `<base>/<app>` | `/var/lib/ob/ledger` |
| Release directory | `<base>/<app>/releases/<release-id>` | `/var/lib/ob/ledger/releases/20260802-183045-a1b2c3d` |
| Host-scoped state | `<base>/_host` | `/var/lib/ob/_host` |

A proxy service SHALL be derived per route, not per workload. A backend carries
the port, so one backend cannot describe a workload that routes on more than
one — every route after the first would overwrite the port of the ones before
it, and the workload would answer on whichever port was declared last. The
first route keeps the workload's own name so a single-routed workload's labels
do not move. Each router SHALL name its backend explicitly, because a router
that does not say which of several services it means is not a router that
selects correctly.

#### Scenario: A workload with several routes gets several backends
- **WHEN** a workload declares routes on more than one port
- **THEN** each route derives its own proxy service carrying its own port, and each router names the one it belongs to

Container names SHALL carry the application component. Container names are
host-global in the container runtime, so a workload-only name such as `server-1`
can collide with a container Onebox does not own that happens to share the name.
Scoping every derived name to the application keeps everything Onebox creates in
one namespace and makes ownership legible from the name alone. Router and proxy
service names SHALL likewise be application-scoped, and SHALL appear in this
table rather than being invented by the implementation, because they become a
permanent generator contract the first time a release ships.

A service's volume identifiers come from its declared `volumes`; a service that
declares none reserves no volume name.

Workload and service identifiers are unique across both blocks, so the workload
and service volume patterns cannot produce the same name for different
resources. The application Compose project is the application identifier alone. It cannot
collide with any derived name because identifiers may not contain underscore and
may not begin `ob-`, which reserves both the underscore-joined namespace and the
two pre-existing hyphenated host-scoped names.

Every derived name, including the transient name a rollout uses before assigning
a stable slot, SHALL be application-scoped. The preflight collision check SHALL
cover every container-runtime object — projects, containers including the
transient one, and volumes — and SHALL NOT cover routers or proxy services,
which are labels rather than runtime objects and cannot collide with a container.

A router's index SHALL carry an `r` prefix. Without it, router 2 of a workload
derives the same string as that workload's second replica container; the two
occupy different namespaces so nothing breaks, but a reader comparing one list
against the other is misled.

Names SHALL be stable across releases so a rollback cannot orphan a resource. A
derived name exceeding sixty-three characters SHALL
be **refused at validation**, naming the offending identifiers and the limit.
Sixty-three characters is an Onebox limit chosen for headroom, not a documented
container-runtime maximum, and the same number applies wherever Onebox derives a
name.

Truncation with a hash suffix was specified and withdrawn: a review produced two
valid workload identifiers whose derived volume names collided under a
seven-character suffix, which falsifies injectivity for exactly the names that
can never be changed afterwards. Lengthening the suffix narrows the window
without closing it. Refusing is total, costs only unusually long identifier
combinations, and relaxing it later is additive.

Volume names SHALL be permanent: once a volume exists, a later release SHALL NOT
derive a different name for the same declared resource.

#### Scenario: Hyphenated identifiers do not collide
- **WHEN** one project declares application `a-b` with service `c`, and another declares application `a` with service `b-c`
- **THEN** their derived service project names differ

#### Scenario: Names are stable across releases
- **WHEN** two different releases of the same project generate a runtime
- **THEN** the project, network, and volume names are identical

#### Scenario: Derived name exceeds the runtime's limit
- **WHEN** a derived name would exceed sixty-three characters
- **THEN** validation fails naming the identifiers and the limit, and no name is truncated

#### Scenario: Foreign container holds a derived name
- **WHEN** a container Onebox does not own already holds a name a release would derive
- **THEN** preflight fails naming the holder, and nothing is renamed, stopped, or removed

#### Scenario: A later release would rename an existing volume
- **WHEN** a change to the naming patterns would derive a different volume name for a resource that already exists
- **THEN** it is a breaking change to this contract and cannot ship without an explicit data-migration path

### Requirement: A job neither restarts nor starts with the application

A workload with the `job` role SHALL be rendered so that the container runtime
does not restart it and does not start it as part of bringing the application
up. A job runs to completion at a release phase or on a schedule, under Onebox's
control; a restart policy would loop it forever, and starting it with the
application would run a migration nobody asked for.

#### Scenario: Job is not restarted
- **WHEN** a job workload is rendered
- **THEN** its restart policy is off

#### Scenario: Job does not start with the application
- **WHEN** the generated runtime is brought up
- **THEN** no job workload is started

### Requirement: Terminating TLS names a certificate resolver

When the proxy terminates TLS, the generated routing SHALL name the certificate
resolver configured on the proxy. Terminating without one yields a router that
never obtains a certificate. The resolver is configured once on the proxy rather
than per route, because one account serves every route.

#### Scenario: Resolver declared
- **WHEN** the proxy declares a certificate resolver and a route terminates TLS
- **THEN** the generated routing names that resolver

#### Scenario: No resolver declared
- **WHEN** no certificate resolver is configured
- **THEN** none is emitted, and the proxy's own default applies

### Requirement: Onebox generates the surrounding runtime

Generation SHALL produce the networks, volumes, and routing the declared
workloads require, deriving routing from each declared route's domain, path,
port, and protocol.

#### Scenario: Routing derived from a declared route
- **WHEN** a workload declares a route with a domain and a port
- **THEN** the generated runtime routes that domain to that port on that workload

#### Scenario: Several routes on one workload
- **WHEN** a workload declares more than one route
- **THEN** every declared route appears in the generated runtime

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

### Requirement: Ejection transfers ownership permanently and atomically

Onebox SHALL write the generated runtime into the repository on request and
repoint the affected workloads at it as Compose references. The written file
SHALL NOT contain the overlay: the ingress network, the `ob.` labels, and the
routing labels SHALL be stripped before writing, so that the ejected file is
ordinary user-authored Compose and the overlay is re-applied on the next
generation like any other Compose reference.

Without stripping, ejection produces a file that its own conflict rules reject —
the ejected service would already carry every key the overlay refuses — and the
project would be unable to generate again. The destination
SHALL default to a Compose file beside the project file and MAY be given
explicitly. Ejection SHALL refuse an existing destination unless overwriting is
explicitly requested.

Ejection SHALL be crash-safe: the runtime file is written and atomically renamed
into place before the project file is rewritten, and re-running ejection after an
interruption SHALL either complete the transfer or refuse with the reason,
never leave the project referencing a file that does not exist.

After ejection Onebox SHALL NOT regenerate or reconcile the ejected services.

Ejection SHALL move only workloads Onebox generates; one that already references
Compose is the user's file already, and moving it would duplicate a service that
lives elsewhere. Ejection SHALL remove from the project the declaration fields
the Compose file now owns, because leaving them lets someone edit a health check
or a volume, see no effect, and get no error. It SHALL preserve the project
file's comments and ordering, and SHALL carry any comment on a replaced source
key onto its replacement rather than deleting it with the line it sat on.

#### Scenario: Ejection writes the runtime and repoints the project
- **WHEN** ejection is requested and no file exists at the destination
- **THEN** the runtime is written, the affected workloads reference it, and they are recorded as user-authored

#### Scenario: Ejection refuses to clobber
- **WHEN** ejection targets an existing path and overwriting was not requested
- **THEN** the operation fails, names the path, and writes nothing

#### Scenario: Interrupted between writing and repointing
- **WHEN** ejection is interrupted after the runtime file is written and before the project is rewritten
- **THEN** the project still references the generator, and re-running ejection completes the transfer or refuses with the reason

#### Scenario: Nothing left to eject
- **WHEN** every workload already references a Compose file
- **THEN** ejection reports that there is nothing to hand over rather than rewriting anything

#### Scenario: The project file survives the rewrite
- **WHEN** a project carrying comments is ejected
- **THEN** its comments and ordering are preserved, and a comment on the replaced source key appears on its replacement

#### Scenario: Inert declaration is removed
- **WHEN** a workload with a health check and volumes is ejected
- **THEN** those fields are removed from the project, because the Compose file now owns them

#### Scenario: Ejected services are not regenerated
- **WHEN** a runtime is generated for a project whose services were previously ejected
- **THEN** the ejected services are used as authored and are not regenerated

#### Scenario: Ejected output is accepted by generation
- **WHEN** a project is ejected and a runtime is generated immediately afterwards
- **THEN** generation succeeds, because the written file carries none of the keys the overlay refuses

### Requirement: Generation binds what execution will run

An executable plan SHALL bind the generated runtime by digest together with the
normalized project, the resolved image references, and the resolved base path.
Execution SHALL refuse a plan whose bound runtime digest does not match the
runtime regenerated from the plan's own inputs.

#### Scenario: Runtime digest is bound
- **WHEN** a plan is created
- **THEN** the plan carries the digest of the generated runtime it will execute

#### Scenario: Regenerated runtime disagrees with the plan
- **WHEN** execution regenerates the runtime and the digest differs from the bound digest
- **THEN** execution is refused before any target mutation and the error directs the operator to re-plan

### Requirement: Generation fails closed

Any generation failure SHALL leave no partial artifact on disk or on the target,
and SHALL be reported with a typed error code and the command that resolves it.

#### Scenario: Failure leaves no partial artifact
- **WHEN** generation fails partway through
- **THEN** no partial runtime is written locally or staged remotely

### Requirement: Service declarations produce no runtime and reserve their names

Generation SHALL NOT emit containers, volumes, or networks for a service
declaration. A service declaration SHALL affect only validation, normalization,
name reservation, and honest reporting until a driver capability exists.

The names a service declaration would derive SHALL nonetheless be reserved:
preflight SHALL refuse a collision against them, so that the driver work later
inherits names nothing else has taken. Reserving a name SHALL NOT create the
resource.

#### Scenario: Service declaration is inert during generation
- **WHEN** a runtime is generated for a project declaring a service
- **THEN** the generated runtime contains nothing for that service, and no container, volume, or network is created

#### Scenario: A service's future name is protected
- **WHEN** a foreign resource already holds the name a declared service would derive
- **THEN** preflight fails and names the collision

#### Scenario: Workload depending on a declared service
- **WHEN** a workload references a declared service Onebox does not run
- **THEN** generation succeeds and reported readiness states that the service is not managed by Onebox

