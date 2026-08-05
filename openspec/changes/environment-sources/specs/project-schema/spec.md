## MODIFIED Requirements

### Requirement: Environment files are per workload, with a project-wide default

Environment values reach containers through **environment sources**, defined in
the requirements below. This requirement is restated in those terms so the
contract carries one mechanism rather than two: the previous form described
plaintext files with per-workload control and a role-restricted default, while
decrypted secrets — the same job, different storage — had no stated rule at all.

`env_files` at any scope SHALL be shorthand for a list of plaintext sources at
that scope. It is retained because a form once accepted is accepted
permanently, and it is not a second mechanism: it declares sources, and every
rule about sources applies to what it declares.

A workload MAY declare its own sources. A project-wide list SHALL apply to
every workload that runs the user's code and declares none. Roles and scopes
are specified in the requirements below.

Sources SHALL be applied in declared order, with a later source overriding an
earlier one. A missing source SHALL fail validation. Preflight checks SHALL
assert that required keys are present and non-empty, and that named keys exist.

Interpolation in referenced Compose sources SHALL be fed by the list resolved
at document scope — the environment's list if it declares one, otherwise the
project's — and never by a workload's own list. Interpolation is a property of
the document: a workload's list cannot supply it without that workload deciding
how another workload's copied service parses. Document scope includes the
environment because the runtime is rendered per environment already; a project
whose environments declare different sources therefore interpolates each with
its own, which is the only reading under which the values a container receives
and the values its document parsed with agree.

The runner's environment SHALL NOT feed it — a document that resolved one way
where it was planned and another way where it runs would differ exactly where
nobody is looking. The resolved values SHALL NOT appear in the generated
runtime or its digest: a referenced source keeps its `${VAR}` verbatim, and the
same sources supply the value again when the container runtime reads the
document on the target.

#### Scenario: Workload list overrides the project list
- **WHEN** a workload declares its own environment files and the project also declares some
- **THEN** only the workload's files are projected into it

#### Scenario: Daemons receive no project-wide files
- **WHEN** a project declares environment files and a daemon declares none
- **THEN** no environment file is projected into the daemon

#### Scenario: Later file wins
- **WHEN** two environment files declare the same key
- **THEN** the value from the later file is used

#### Scenario: Interpolation resolves from the project-wide list
- **WHEN** a referenced Compose source uses `${VAR}` and a project-wide environment file declares it
- **THEN** the value is used when the document is parsed, and the generated runtime still carries `${VAR}` verbatim

#### Scenario: Interpolation follows the environment's list when it declares one
- **WHEN** an environment declares its own sources and a referenced Compose source uses `${VAR}`
- **THEN** that environment's deploy interpolates from its own list, so the parsed document and the container's values agree

#### Scenario: The runner's environment is not consulted
- **WHEN** a variable a referenced source uses is set in the runner's own environment and in no declared file
- **THEN** it resolves empty, exactly as it would on the target

#### Scenario: A required variable nobody supplies is refused
- **WHEN** a referenced source declares `${VAR:?}` and no declared environment file supplies it
- **THEN** the failure names the variable and the environment files that feed interpolation, and does not report it as a defect in generated output

### Requirement: Environment overrides are closed, merge predictably, and win

An environment MAY override only these fields:

| Target | Overridable |
|---|---|
| Workload | `replicas`, `resources`, `env`, `strategy`, `routes`, `sources` |
| Service | `resources`, `settings` |

A field is overridable only if changing it cannot change which artifact runs or
what it does to data. `build`, `image`, `compose`, `command`, `run`,
`data_effect`, `volumes`, `persistence`, `driver`, and `version` SHALL therefore
be refused, as SHALL any field not listed.

`sources` is overridable because without it a workload that declares its own
sources can never vary them by environment — and a daemon receives sources only
by declaring them, so every daemon holding a credential would be pinned to one
environment's values across all of them. That is the same failure this contract
elsewhere refuses: one environment's credentials reaching another with nothing
saying so. Which sources a workload reads cannot change which artifact runs or
what it does to data, so it satisfies the rule for admission.

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
- **WHEN** an environment override sets a key to null
- **THEN** that key is absent from the resolved configuration

#### Scenario: List override replaces
- **WHEN** an environment overrides a list-valued field
- **THEN** the resolved list is the override's list, not a concatenation

#### Scenario: Override outside the closed set
- **WHEN** an environment overrides a field not in the permitted set
- **THEN** validation fails naming the field and listing what may be overridden

#### Scenario: Override introduces a workload
- **WHEN** an environment override names a workload the project does not declare
- **THEN** validation fails naming that workload

#### Scenario: A daemon's sources vary by environment
- **WHEN** a daemon declares sources and an environment overrides them
- **THEN** that environment's deploy gives the daemon the overridden sources and no other environment's values

## ADDED Requirements

### Requirement: An environment source is one named contributor of values

A project MAY declare named environment sources. A source is a file that
contributes environment values to containers.

Whether a source is plaintext or encrypted SHALL be a property of the source
and not of the workload receiving it. A SOPS-encrypted source is decrypted when
a release is staged; a plaintext source is read as it is. Nothing else about
how a source behaves SHALL depend on which of the two it is.

This is stated because the contract previously carried two mechanisms —
environment files and secrets — with different scoping rules, different
per-workload control, and different defaults, for the same job of getting
values into a container. Two mechanisms for one idea is how the rules came to
disagree.

Sources SHALL be declared as an ordered list, not as a named set to choose
from. A list of sources means every one of them applies, in the order written.

This follows the container runtime's own `env_file`, which takes a list and
applies it in order. Adopting different semantics for the same idea would mean
a project file and the runtime it generates disagreed about what a list of
files means.

The upstream shape it has to serve is one file shared widely: authentik gives a
single `.env` to its server, its worker and its Postgres; Immich gives one to
its server and its machine-learning container. Sharing, not selecting, is what
these deployments do.

The distinction is load-bearing. The contract previously declared secrets as a
named map, which reads as a set to select from, and the implementation duly
selected — taking whichever name sorted first. A list cannot be selected from
by accident, because there is nothing to select.

#### Scenario: A source is declared and used regardless of its kind
- **WHEN** a project declares one plaintext source and one SOPS source and a workload resolves both
- **THEN** both contribute values, in declared order, and neither is treated specially because of its kind

#### Scenario: Several sources compose
- **WHEN** a project declares several sources
- **THEN** every one applies in the order written, and none is chosen over another

### Requirement: Every workload resolves an ordered list of sources

A workload's environment sources SHALL be resolved from the most specific
declaration available: the workload's own list if it declares one, otherwise
the environment's list if it declares one, otherwise the project's list.

A declared empty list SHALL mean the workload receives no sources. An absent
list SHALL mean the workload takes the next declaration in that order. The
distinction SHALL be observable in the canonical form, because "declared none"
and "did not say" are different intents and the failure they produce differs.

Where an environment needs different values, it SHALL declare its own list.
Varying by scope is the whole mechanism: there is no rule that inspects a
source's name, matches it against the environment being deployed, or picks
between candidates. A previous implementation did pick — the alphabetically
first — and that is how a project came to ship production's credentials to
staging. Under an ordered list resolved by scope there is no pick to get wrong.

#### Scenario: A workload's own list wins
- **WHEN** a workload declares sources and the project also declares some
- **THEN** only the workload's sources are resolved for it

#### Scenario: An environment's list applies to its deploy
- **WHEN** an environment declares sources and a workload declares none
- **THEN** the environment's sources are resolved for that workload on that environment's deploys

#### Scenario: Environments select different sources
- **WHEN** two environments each select a different source by name
- **THEN** each deploy carries only the source its environment selected

#### Scenario: A workload declines every source
- **WHEN** a workload declares an empty list of sources
- **THEN** it receives none, even though the project declares some

### Requirement: The default reaches the application's own workloads

Where a workload declares no sources of its own, the resolved list SHALL apply
to it if its role is `application`, `worker` or `job`, and SHALL NOT apply to a
`daemon`.

The reason is what the configuration is, not who wrote the container.
Application-scoped configuration — the credentials and settings the application
needs to do its work — SHALL NOT reach infrastructure containers by default; a
`daemon` is infrastructure the application depends on, and its configuration is
its own.

An earlier form justified this as "a daemon is a server you author rather than
code you wrote". That is false for the population that uses this contract:
across every third-party conversion in the corpus, no container is code the
author wrote. It is also circular — Immich's machine-learning container appears
as a `daemon` in one conversion and a `worker` in another, the same image in
both, and the deciding factor is whether it should receive the environment. A
principle that is chosen by the outcome it is supposed to decide cannot settle
the next case.

A workload's source — `image`, `build` or `compose` — SHALL NOT affect what it
receives. Only its role does. An application adopted verbatim from a Compose
file SHALL receive what an application receives; previously it received
nothing, which nothing stated and nobody chose.

A `daemon` MAY name sources explicitly and SHALL receive them when it does, so
that a server legitimately needing a credential can be given one.

Naming it is required rather than automatic, and the cost is real: the common
upstream layout shares one file across every service, so converting authentik
or Immich means naming the source on the database too. That cost is accepted
deliberately. Projecting every source into every container would put the
application's third-party credentials inside its database, and a default that
is wrong in the safe direction is recoverable — the container fails to start
and says which variable is missing — while a default that is wrong in the other
direction is not observable at all.

#### Scenario: A job receives the default
- **WHEN** a project declares sources and a job declares none
- **THEN** the sources are resolved for the job, because a job runs the user's code

#### Scenario: A compose-sourced application receives the default
- **WHEN** an application is sourced from a referenced Compose service and declares no sources
- **THEN** it receives the same sources an image-sourced application would

#### Scenario: A daemon receives nothing by default
- **WHEN** a project declares sources and a daemon declares none
- **THEN** no source is resolved for the daemon

#### Scenario: A daemon may be given a source
- **WHEN** a daemon names a source explicitly
- **THEN** that source is resolved for it

### Requirement: Composition order is stated, and generated credentials win

The value a container receives SHALL be determined by this order, lowest
precedence first:

1. Values the referenced Compose file carries, for a `compose:`-sourced workload
2. Resolved sources, in declared order, later overriding earlier
3. Managed-service connections
4. The workload's inline `env`

Inline `env` sits highest because the container runtime places `environment`
above `env_file` and a generated runtime cannot contradict the runtime that
reads it. The previous form of this requirement stopped at (2) and (3), claimed
connections could not be shadowed, and was false: every fixture in the corpus
uses inline `env`, and inline `env` outranks both.

The claim is made true by refusal rather than by ordering. A workload SHALL NOT
set, in its inline `env`, a variable that a managed-service connection supplies
to it; declaring one SHALL fail validation naming the variable and the service.
A rule that cannot be enforced by the layer it describes has to be enforced by
the layer above it.

Connections supply the credential parts that cannot be authored, because a
credential generated on the target exists nowhere else. They do not own the
endpoint. A workload MAY map only the parts it wants — the per-part mapping
already in this contract — and author host, port or database itself, which is
what pointing an application at a connection pooler or a read replica requires.

A workload whose application reads a single connection URL cannot do this: the
URL carries the credential, the credential never travels, and no authored value
can reconstruct it. That limitation SHALL be stated in the authoring guide
rather than discovered, and it bounds the endpoint-override mechanism to
applications that read connection parts separately.

Values SHALL NOT appear in the project file, the generated runtime, any plan
artifact, or any structured output, whatever their source's kind. A plaintext
source is not less sensitive than an encrypted one; it is only stored
differently.

#### Scenario: A later source overrides an earlier one
- **WHEN** two resolved sources declare the same key
- **THEN** the value from the later source is used

#### Scenario: A generated credential cannot be shadowed
- **WHEN** a declared source sets a variable a managed-service connection also sets
- **THEN** the connection's value is the one the container receives

#### Scenario: Inline env colliding with a connection is refused
- **WHEN** a workload's inline `env` sets a variable a managed-service connection supplies to it
- **THEN** validation fails naming the variable and the service, because inline env would otherwise outrank the connection

#### Scenario: An endpoint may be authored while the credential is generated
- **WHEN** a workload maps only the user and password parts of a connection and authors the host itself
- **THEN** it reaches the authored host with the generated credential

#### Scenario: No value reaches an artifact
- **WHEN** a project resolving any source is rendered, planned, or printed in any structured form
- **THEN** no value from any source appears in the output
