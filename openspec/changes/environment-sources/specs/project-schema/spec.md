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

A source name SHALL be an identifier and SHALL NOT imply anything about where
it is used. In particular a source named after an environment SHALL NOT be
selected because an environment of that name is being deployed; selection is
explicit and is defined below.

#### Scenario: A source is declared and used regardless of its kind
- **WHEN** a project declares one plaintext source and one SOPS source and a workload resolves both
- **THEN** both contribute values, in declared order, and neither is treated specially because of its kind

#### Scenario: A source name is not a selector
- **WHEN** a project declares a source named `staging` and the `staging` environment is deployed
- **THEN** the source is used only if something selects it by name

### Requirement: Every workload resolves an ordered list of sources

A workload's environment sources SHALL be resolved from the most specific
declaration available: the workload's own list if it declares one, otherwise
the environment's list if it declares one, otherwise the project's list.

A declared empty list SHALL mean the workload receives no sources. An absent
list SHALL mean the workload takes the next declaration in that order. The
distinction SHALL be observable in the canonical form, because "declared none"
and "did not say" are different intents and the failure they produce differs.

Selection SHALL be by name. Where a project declares more than one source and
nothing selects between them, loading SHALL fail naming the sources, rather
than choosing one. Choosing silently is how a project came to ship one
environment's credentials to another.

#### Scenario: A workload's own list wins
- **WHEN** a workload declares sources and the project also declares some
- **THEN** only the workload's sources are resolved for it

#### Scenario: An environment's list applies to its deploy
- **WHEN** an environment declares sources and a workload declares none
- **THEN** the environment's sources are resolved for that workload on that environment's deploys

#### Scenario: Environments select different sources
- **WHEN** two environments each select a different source by name
- **THEN** each deploy carries only the source its environment selected

#### Scenario: An ambiguous selection is refused
- **WHEN** a project declares more than one source and no environment or workload selects between them
- **THEN** loading fails naming the declared sources, and no source is chosen by ordering

#### Scenario: A workload declines every source
- **WHEN** a workload declares an empty list of sources
- **THEN** it receives none, even though the project declares some

### Requirement: The default reaches the workloads that run the user's code

Where a workload declares no sources of its own, the resolved list SHALL apply
to it if it runs the user's code — roles `application`, `worker` and `job` —
and SHALL NOT apply to a `daemon`, which is a server the user authors rather
than code they wrote and whose configuration belongs in its own `env`.

A workload's source — `image`, `build` or `compose` — SHALL NOT affect what it
receives. Only its role does. An application adopted verbatim from a Compose
file SHALL receive what an application receives; previously it received
nothing, which nothing stated and nobody chose.

A `daemon` MAY name sources explicitly and SHALL receive them when it does, so
that a server legitimately needing a credential can be given one.

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

Resolved sources SHALL be applied in order, with a later source overriding an
earlier one. Managed-service connections SHALL be applied after every declared
source, because a credential generated on the target is the only thing that can
open the service it describes and nothing authored may shadow it.

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

#### Scenario: No value reaches an artifact
- **WHEN** a project resolving any source is rendered, planned, or printed in any structured form
- **THEN** no value from any source appears in the output
