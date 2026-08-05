## MODIFIED Requirements

### Requirement: The field model is normative and machine-checkable

The `onebox.run/v1` contract SHALL be enforced by a machine-checkable schema
that admits exactly the documented shapes and rejects everything else. The
schema SHALL be closed: a field the contract does not define SHALL be rejected,
naming the field and the line that declared it.

Closedness SHALL be a property of the type the document decodes into, not a
semantic derived from how definitions compose. A schema that reports a field as
accepted while the contract does not define it is a defect of the highest
order: it removes the guarantee the contract exists to provide, and it does so
without any output.

A rejected name that is within a small edit distance of a defined one SHALL
suggest that name. A rejected value SHALL name the field, the line, and the
constraint it failed, and SHALL NOT expose the vocabulary of whatever performs
the checking.

#### Scenario: An undefined field is rejected
- **WHEN** a project declares a field the contract does not define
- **THEN** loading fails, naming that field and the line it appears on

#### Scenario: A near-miss name is corrected
- **WHEN** a rejected field name is within a small edit distance of a defined one
- **THEN** the failure suggests the defined name

#### Scenario: Closedness holds for every role
- **WHEN** an undefined field is declared on a workload of any role
- **THEN** it is rejected, and the failure names the field rather than the role

#### Scenario: Loader-enforced rule is still enforced
- **WHEN** a project declares top-level shorthand alongside a `workloads` block
- **THEN** loading fails naming both locations, even though the field model alone accepts it

#### Scenario: Two implementations agree
- **WHEN** two implementations are given the conformance corpus
- **THEN** they accept and reject the same projects and produce the same canonical form

#### Scenario: Schema and prose disagree
- **WHEN** a prose statement in this specification conflicts with the enforced model
- **THEN** the model governs and the prose is a defect to be corrected

#### Scenario: Default is applied and attributed
- **WHEN** a project omits a field carrying a default
- **THEN** the canonical form contains the documented default and reports its origin as a default

### Requirement: A machine-readable schema is published for editors

Each release SHALL publish a JSON Schema for the authoring contract, generated
from the same declarations the loader enforces, and SHALL embed it in the binary
and write it to a repository path on request. Scaffolding SHALL reference it
from the project file's first line.

The published schema SHALL be gated against the conformance corpus: it SHALL
accept and reject exactly what the loader accepts and rejects, and a divergence
SHALL fail the build. A published schema that disagrees with the enforced one
teaches an author something untrue and is worse than publishing nothing.

#### Scenario: Published schema matches the enforced contract
- **WHEN** the published schema is run against the conformance corpus
- **THEN** it accepts and rejects exactly what the loader does

#### Scenario: Scaffolded project references the schema
- **WHEN** a scaffolded project is opened in an editor supporting `yaml-language-server`
- **THEN** the schema resolves and completion, hover documentation, and inline errors are available

## ADDED Requirements

### Requirement: Replacing the validator changes nothing an author can observe

Replacing the validation implementation SHALL NOT change what the contract
accepts, what it rejects, or what it generates. The conformance corpus SHALL
produce an identical verdict for every case, every project in the conversion
corpus SHALL load, and the runtime generated for each SHALL be byte-identical to
the runtime generated before the replacement.

A divergence SHALL be treated as a defect in the replacement, never as a
decision. A constraint that is inconvenient to reproduce SHALL be reproduced.

#### Scenario: The corpus verdict does not move
- **WHEN** the conformance corpus runs against the replaced validator
- **THEN** every case produces the same accept or reject as before

#### Scenario: Generated runtimes do not move
- **WHEN** each corpus project is rendered before and after the replacement
- **THEN** the generated runtimes are byte-identical

### Requirement: Defaults are applied explicitly

Every default the contract declares SHALL be applied by an explicit assignment
whose result is observable in the canonical form, and SHALL be reported as
derived rather than as authored.

Defaulting SHALL NOT depend on whether a field was declared optional. That
coupling produced a defect in this contract once — declared defaults that never
materialised — and an explicit assignment cannot fail that way.

#### Scenario: Every declared default materialises
- **WHEN** a project omits a field that has a default
- **THEN** the canonical form shows the default value, marked as derived
