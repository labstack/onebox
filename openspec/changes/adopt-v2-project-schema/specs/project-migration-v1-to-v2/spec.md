## Purpose

Defines how an existing `onebox.run/v1` project reaches the v2 contract without
an unplanned outage or a silent change of meaning, by keeping v1 loadable for a
bounded window and by converting mechanically while refusing to guess at anything
the v1 project did not state.

## ADDED Requirements

### Requirement: v1 projects remain loadable for one release cycle

Onebox SHALL continue to load, validate, plan, and execute `onebox.run/v1`
projects for one release cycle after v2 ships, with unchanged behavior. Every
command that loads a v1 project SHALL emit a deprecation notice naming the
conversion command and the release in which v1 support ends. The notice SHALL be
written to the diagnostic stream and SHALL NOT corrupt structured output.

#### Scenario: v1 project still deploys
- **WHEN** a v1 project is planned and executed during the deprecation window
- **THEN** behavior is unchanged from before v2 shipped

#### Scenario: Deprecation notice does not break automation
- **WHEN** a v1 project is loaded with structured output requested
- **THEN** the deprecation notice appears on the diagnostic stream and the structured stream remains parseable

#### Scenario: Support ends
- **WHEN** a v1 project is loaded by a release after the deprecation window
- **THEN** loading fails and the error names the conversion command

### Requirement: Conversion is mechanical and never invents meaning

Conversion SHALL read a v1 project together with the Compose file it classifies
and emit a v2 project that preserves the declared meaning. A v1 component
classified as an application, worker, or job SHALL become a workload. A v1
component classified as a data service SHALL become a service declaration when a
driver name exists for its type, and a workload otherwise. Conversion SHALL NOT
infer persistence semantics, data effects, migration compatibility, backup
posture, or destructive tolerance that the v1 project did not state.

#### Scenario: Application component converts to a workload
- **WHEN** a v1 project classifies a component as an application
- **THEN** the converted project declares a workload carrying that component's rollout, readiness, and drain settings

#### Scenario: Recognized data service converts to a service declaration
- **WHEN** a v1 project classifies a component with a type for which a driver name exists
- **THEN** the converted project declares a service of that driver, and the service is reported as declared and not managed

#### Scenario: Unrecognized data service converts to a workload
- **WHEN** a v1 project classifies a component whose type has no driver name
- **THEN** the converted project declares a workload sourced from the original Compose service

#### Scenario: Unstated semantics are not invented
- **WHEN** a v1 project does not state a persistence mode or data effect
- **THEN** conversion does not assign one and reports it as requiring a decision

### Requirement: Conversion reports what it could not convert

Conversion SHALL report every construct it could not represent, naming the
construct and its source location, and SHALL classify each as requiring a
decision or as preserved through a Compose reference. Conversion SHALL NOT
silently discard any declared construct.

#### Scenario: Construct requires a decision
- **WHEN** a v1 construct has no v2 equivalent and cannot be preserved by reference
- **THEN** conversion reports it, names its location, and marks the resulting project as requiring a decision before use

#### Scenario: Construct preserved by reference
- **WHEN** a Compose service carries settings the v2 contract cannot express
- **THEN** conversion emits a workload that references that Compose service and reports the preservation

#### Scenario: Nothing is discarded silently
- **WHEN** conversion completes
- **THEN** every construct in the source project appears in the converted project or in the report

### Requirement: Conversion never touches production and never clobbers

Conversion SHALL be a local operation that contacts no target and mutates no
remote state. It SHALL refuse to overwrite an existing destination file unless
overwriting is explicitly requested, and on any failure SHALL leave the source
project and the destination unchanged.

#### Scenario: Conversion contacts no target
- **WHEN** a project is converted
- **THEN** no target connection is attempted and no remote state changes

#### Scenario: Destination already exists
- **WHEN** conversion targets an existing file and overwriting was not requested
- **THEN** the operation fails, names the path, and writes nothing

#### Scenario: Failure leaves inputs intact
- **WHEN** conversion fails partway through
- **THEN** the source project is unchanged and no partial destination file remains

### Requirement: A converted project is verifiable against its source

Conversion SHALL support comparing the runtime generated from the converted
project against the runtime the source v1 project produces, and SHALL report
every difference. A difference SHALL be reported rather than resolved
automatically, so that an operator or agent decides whether it is intended.

#### Scenario: Equivalent conversion
- **WHEN** a converted project generates a runtime equivalent to the v1 project's runtime
- **THEN** the comparison reports no differences

#### Scenario: Divergent conversion
- **WHEN** the generated runtimes differ
- **THEN** every difference is reported and none is resolved automatically

### Requirement: Conversion is agent-operable

Conversion SHALL support a versioned structured output mode reporting the
destination, the constructs requiring a decision, the constructs preserved by
reference, and the comparison result. Failures SHALL carry a typed error code and
the command that resolves them. Structured output SHALL contain no plaintext
secret values.

#### Scenario: Structured conversion report
- **WHEN** conversion runs with structured output requested
- **THEN** the output carries a versioned envelope listing decisions required, preservations, and the comparison result

#### Scenario: Secrets absent from the report
- **WHEN** a converted project references secret values
- **THEN** the structured report contains references and no resolved secret content
