## MODIFIED Requirements

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

The guarantee SHALL attach at the first published release of a binary accepting
this identity. Before that release the contract MAY be revised incompatibly
under the same identity; each such revision SHALL re-freeze the conformance
corpus and enumerate every verdict and every generated runtime that moved. A
moved verdict absent from that record SHALL be a defect. This clause SHALL be
removed by the change that publishes the first release.

The guarantee protects projects an author already deployed against a released
binary. Before any release that set is empty, and a rule whose protected set is
empty cannot be the reason to preserve behaviour the contract itself calls
wrong. Stating the gate is what keeps this from being a quiet exception: the
alternative is a contract that claims additivity while changes land that are
not additive, which costs more than the guarantee is worth.

#### Scenario: Later release adds an optional field
- **WHEN** a later release adds an optional field and a project omits it
- **THEN** the project's meaning is unchanged and it validates as before

#### Scenario: Older runner meets a project that requires a newer one
- **WHEN** a project declares a minimum runner version newer than the running binary
- **THEN** validation fails

#### Scenario: Narrowing is refused
- **WHEN** a change would cause a previously accepted project to be rejected
- **THEN** it cannot ship under this identity

#### Scenario: Pre-release revision is loud, not quiet
- **WHEN** a pre-release change alters the meaning of a project the corpus accepts
- **THEN** the change re-freezes the corpus and its record enumerates every moved verdict

#### Scenario: The gate closes at first release
- **WHEN** the first release has been published
- **THEN** a change that would reject a previously accepted project, or change what it means, cannot ship under this identity

### Requirement: Environment files are per workload, with a project-wide default

Environment values SHALL reach containers through `env_files`. An entry SHALL be
a repository path, an object `{file: <path>}`, or an object
`{file: <path>, provider: <provider>}` where the provider set is closed and
currently contains only `sops`. Whether an entry is plaintext or encrypted SHALL
be a property of the entry; nothing else about how it behaves SHALL depend on
which it is.

One field carries both because they do one job. The contract previously carried
`env_files` for plaintext and a separate `secrets` block for encrypted values,
with different scoping, different per-workload control, and — for secrets — no
stated rule at all. Two mechanisms for one idea is how their rules came to
disagree.

The field SHALL be accepted at exactly three scopes: `runtime.env_files`,
`environments.<name>.env_files`, and `workloads.<name>.env_files`, and in a
workload override. A list SHALL mean every entry applies in the order written,
a later entry overriding an earlier one where both declare a key. There SHALL
be no rule that selects among entries, matches an entry against the
environment's name, or prefers one entry to another for any reason other than
order.

A declared entry whose file does not exist SHALL fail validation naming the
file. Preflight checks SHALL assert that required keys are present and
non-empty, and that named keys exist.

A top-level `secrets` block SHALL be refused, naming the entry form that
replaces it. Reinterpreting its keys as environment names was considered and
rejected: today those keys are arbitrary names, so a project declaring
`secrets: {app: …}` with no environment called `app` would silently change from
applying to never applying — the exact class of failure this contract exists to
remove.

Interpolation in referenced Compose sources SHALL be fed by the list resolved at
document scope — the environment's list if it declares one, otherwise the
project's — never by a workload's list and never by the runner's environment.
Entries feed it regardless of kind, so a command that must parse a referenced
document interpolating from an encrypted entry SHALL fail naming the entry when
it cannot decrypt it, rather than resolving the variable empty. Resolved values
SHALL NOT appear in the generated runtime or its digest: `${VAR}` stays verbatim
and the same entries supply the value again on the target.

#### Scenario: Workload list overrides the project list
- **WHEN** a workload declares its own environment files and the project also declares some
- **THEN** only the workload's files are projected into it

#### Scenario: Daemons receive no project-wide files
- **WHEN** a project declares environment files and a daemon declares none
- **THEN** no environment file is projected into the daemon

#### Scenario: Later file wins
- **WHEN** two environment files declare the same key
- **THEN** the value from the later file is used

#### Scenario: Several entries compose in order
- **WHEN** a scope declares a plaintext entry, an encrypted entry, and another plaintext entry
- **THEN** all three apply in the order written, the encrypted one occupying exactly its declared position

#### Scenario: Nothing is selected
- **WHEN** a project declares two encrypted entries
- **THEN** both apply in order, and neither is chosen over the other for any reason including its name

#### Scenario: The withdrawn secrets block is refused with direction
- **WHEN** a project declares a top-level `secrets` block
- **THEN** loading fails naming the block and the entry form that replaces it, rather than reporting an unknown field

#### Scenario: Interpolation resolves from the project-wide list
- **WHEN** a referenced Compose source uses `${VAR}` and a project-wide environment file declares it
- **THEN** the value is used when the document is parsed, and the generated runtime still carries `${VAR}` verbatim

#### Scenario: Interpolation follows the environment's list when it declares one
- **WHEN** an environment declares its own list and a referenced Compose source uses `${VAR}`
- **THEN** that environment's deploy interpolates from its own list

#### Scenario: An encrypted entry that cannot be decrypted fails loudly at interpolation
- **WHEN** only an encrypted document-scope entry supplies a variable a referenced source interpolates, and decryption is unavailable
- **THEN** the failure names the entry, and the variable is not silently resolved empty

#### Scenario: The runner's environment is not consulted
- **WHEN** a variable a referenced source uses is set in the runner's own environment and in no declared file
- **THEN** it resolves empty, exactly as it would on the target

#### Scenario: A required variable nobody supplies is refused
- **WHEN** a referenced source declares `${VAR:?}` and no declared environment file supplies it
- **THEN** the failure names the variable and the environment files that feed interpolation

### Requirement: Environment overrides are closed, merge predictably, and win

An environment MAY override only these fields:

| Target | Overridable |
|---|---|
| Workload | `replicas`, `resources`, `env`, `strategy`, `routes`, `env_files` |
| Service | `resources`, `settings` |

A field is overridable only if changing it cannot change which artifact runs or
what it does to data. `build`, `image`, `compose`, `command`, `run`,
`data_effect`, `volumes`, `persistence`, `driver`, and `version` SHALL therefore
be refused, as SHALL any field not listed.

`env_files` qualifies: which files a workload reads cannot change which artifact
runs or what it does to data. Without it a workload declaring its own list —
which every daemon holding a credential must — is pinned to one environment's
values across all of them, which is the failure this contract elsewhere refuses.

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

#### Scenario: A daemon's list varies by environment
- **WHEN** a daemon declares `env_files` and an environment overrides them
- **THEN** that environment's deploy gives the daemon the overridden list and no other environment's values

## ADDED Requirements

### Requirement: Every workload resolves one list, from the most specific declaration

A workload's list SHALL be resolved from the most specific declaration present:
the environment's override for that workload; otherwise the workload's own list;
otherwise the environment's list; otherwise the project's list.

A declared empty list SHALL mean the workload receives no entries. An absent
list SHALL mean the next declaration in that order applies. The two SHALL be
distinguishable in the canonical form, each carrying its origin, because
"declared none" and "did not say" are different intents.

#### Scenario: The workload's own list beats the environment's default
- **WHEN** a workload declares a list and the environment also declares one
- **THEN** only the workload's list resolves for it

#### Scenario: Environments carry different values
- **WHEN** two environments each declare their own list and a workload declares none
- **THEN** each deploy resolves only its own environment's list, and neither runtime carries the other's values

#### Scenario: Declining is expressible
- **WHEN** a workload declares an empty list
- **THEN** it receives no entries although the project declares some, and the canonical form shows the empty list as explicit

#### Scenario: An override replaces the resolved list wholesale
- **WHEN** an environment overrides a workload's list
- **THEN** the override's list replaces whatever would otherwise resolve, with no concatenation

### Requirement: The default reaches the application's own workloads, and the source is never a factor

Where the default applies — the workload declares no list and is not overridden
— it SHALL apply to roles `application`, `worker` and `job`, and SHALL NOT apply
to a `daemon`.

Application-scoped configuration SHALL NOT reach infrastructure containers by
default. A `daemon` is infrastructure the application depends on, and its
configuration is its own. A default wrong in that direction is unobservable; a
default wrong in the safe direction fails visibly at startup naming the missing
variable.

The role gate SHALL govern only the default. A `daemon` MAY declare its own list
or be given one by an override, and SHALL receive exactly that.

A workload's source — `image`, `build` or `compose` — SHALL NOT affect what it
receives. The compose path SHALL take the same resolution branch as the image
path.

#### Scenario: A job receives the default
- **WHEN** a project declares a list and a job declares none
- **THEN** the list resolves for the job

#### Scenario: A compose-sourced application receives what an image-sourced one receives
- **WHEN** an application sourced from a Compose reference declares no list
- **THEN** it resolves exactly the list an image-sourced application in its position would

#### Scenario: A daemon defaults to nothing but may be given a list
- **WHEN** a project declares a default, one daemon declares a list and another declares none
- **THEN** the first receives its list and the second receives nothing

### Requirement: Composition order is the container runtime's, and generated credentials are unshadowable by refusal

The effective value of a variable SHALL be determined by, lowest precedence
first:

1. the referenced Compose service's own `env_file` entries, for a
   `compose:`-sourced workload
2. the workload's resolved `env_files` entries, in order
3. managed-service connection files
4. the service's `environment` level — the workload's inline `env` for a
   generated workload, the referenced service's own `environment` keys for a
   compose-sourced one

Levels 1 to 3 SHALL be delivered as the generated `env_file` list in that order.
Level 4 outranks them because the container runtime places `environment` above
`env_file`, and a generated runtime SHALL NOT contradict the runtime that reads
it.

Level 4 shadowing a level 2 entry is legitimate: it is the most specific
authored statement about that container. Level 4 shadowing a level 3 connection
variable SHALL fail validation, naming the variable, the service, and for a
compose source the file. A credential generated on the target exists nowhere
else, so nothing authored may claim its name — and because ordering cannot
enforce that, refusal does.

Connections own the credential parts and not the endpoint. A workload MAY map
only the parts it wants and author host, port or database itself, which is what
pointing an application at a connection pooler or a read replica requires. An
application reading a single connection URL cannot do this, because the URL
carries the credential and the credential never travels; that limitation SHALL
be stated in the authoring guide rather than discovered.

No value from any entry, whatever its kind, SHALL appear in the project file,
the generated runtime, any plan artifact, or any structured output.

#### Scenario: A connection file outranks a declared entry
- **WHEN** a resolved entry sets a variable a connection file also sets
- **THEN** the container receives the connection's value

#### Scenario: Inline env claiming a connection variable is refused
- **WHEN** a workload's inline `env` sets a variable a managed-service connection supplies to it
- **THEN** validation fails naming the variable and the service

#### Scenario: A referenced service's environment key claiming a connection variable is refused
- **WHEN** a compose-sourced workload needs a managed service and the referenced service's `environment` sets a variable the connection supplies
- **THEN** generation fails naming the variable, the service and the referenced file

#### Scenario: A referenced service's environment key beats a projected entry
- **WHEN** a compose-sourced workload's referenced `environment` and a resolved entry both declare a key no connection supplies
- **THEN** the referenced value is the one the container receives, and nothing is refused

#### Scenario: Plaintext is redacted like everything else
- **WHEN** a project resolving plaintext entries is rendered, planned, or printed in structured form
- **THEN** no value from any entry appears in the output

### Requirement: A decrypted entry is staged with the release and outlives the deploy

Each encrypted entry SHALL be decrypted into a per-entry file inside the release
directory when the release is staged, occupying that entry's position in the
generated `env_file` list.

Staged decrypted files SHALL persist with the release, because a scheduled job
fires from the host's own timer with no Onebox process alive — including after a
reboot — and must resolve the values the deploy resolved.

#### Scenario: A timer-fired job resolves its values after a reboot
- **WHEN** a scheduled job whose list includes an encrypted entry fires after the host reboots
- **THEN** the container receives the values staged with the current release
