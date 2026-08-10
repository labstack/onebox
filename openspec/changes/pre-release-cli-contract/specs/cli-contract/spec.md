## Purpose

Defines one compact, deterministic CLI contract for both humans and coding agents, independent of which command or owned resource is being operated.

## ADDED Requirements

### Requirement: Global configuration selection is uniform

Every command that reads a project SHALL resolve the project from the global `--config` value, relative paths inside it SHALL resolve from that project's directory, and no command SHALL silently fall back to `ob.yml` after a different path was supplied.

#### Scenario: Eject uses an explicit project
- **WHEN** `ob eject --config path/to/project.yml` is invoked
- **THEN** the named project is read and rewritten and a sibling default `ob.yml` is neither required nor touched

#### Scenario: Explicit project is missing
- **WHEN** a command receives a missing `--config` path
- **THEN** it returns a typed project error naming that exact path and any semantically appropriate diagnostic, next-step, or resolving command

### Requirement: Finite commands share one structured envelope

Every finite, non-passthrough command SHALL support a JSON envelope containing `schema_version`, `command`, `outcome`, and exactly one of `data` or `error`. `outcome` SHALL be one of `success`, `no_op`, `cancelled`, or `error`. An error SHALL contain a stable typed `code`, `safe_message`, and any semantically applicable `diagnostic_command`, `next_command`, or `resolving_command`; it SHALL NOT collapse a known cause into only `operation_failed`.

Structured records SHALL be the only bytes written to stdout in a structured mode. Human diagnostics and trusted local detail SHALL use stderr. Exit status SHALL be 0 for `success` and `no_op`, 2 for `cancelled`, and 1 for `error`.

#### Scenario: Preflight is requested as JSON
- **WHEN** an agent invokes preflight with `--output json`
- **THEN** it receives a versioned result containing checks and a typed error when any check fails

#### Scenario: Structured mutation fails
- **WHEN** a mutation fails for a known validation, binding, approval, recovery, or provider cause
- **THEN** the structured result preserves that specific error code and assigns guidance to the correct diagnostic, next-step, or resolving role

#### Scenario: Structured output contains sensitive inputs
- **WHEN** a command resolves encrypted or plaintext environment entries
- **THEN** the structured result contains references or opaque generation identifiers and no resolved value or raw secret-content hash

### Requirement: Streaming and passthrough commands have explicit modes

Commands that stream operational events SHALL support NDJSON records containing `schema_version`, `command`, monotonic `sequence`, `kind`, and event data, followed by exactly one terminal record carrying the common outcome or error. Unbounded commands such as `logs --follow` SHALL refuse JSON and direct the caller to NDJSON. Finite logs MAY use JSON or NDJSON.

Non-terminal operation progress SHALL use status `running`. Every public terminal operation status and result outcome SHALL use the same closed vocabulary as the common envelope: `success`, `no_op`, `cancelled`, or `error`; alternate spellings such as `started`, `succeeded`, and `failed` SHALL NOT appear in the public contract.

`exec` output and container logs are operator-controlled passthrough data and MAY contain secrets; Onebox SHALL NOT claim to redact them. In NDJSON mode their chunks SHALL be identified as stdout or stderr without Onebox adding credentials. Interactive editor commands SHALL remain trusted-terminal operations and return only their terminal result envelope after the editor exits. Help and completion SHALL remain Cobra/shell-native protocol output and SHALL not be wrapped as operation results.

#### Scenario: Follow logs requests JSON
- **WHEN** `logs --follow --output json` is invoked
- **THEN** the command returns typed error `output_mode_incompatible` directing the caller to `--output ndjson`

#### Scenario: Streaming command terminates
- **WHEN** an NDJSON operation stream ends by success, cancellation, or error
- **THEN** exactly one terminal record is emitted after all output and progress records

#### Scenario: Exec emits operator-controlled output
- **WHEN** an executed command prints sensitive bytes
- **THEN** Onebox marks the stream and does not add credentials, while documentation states that passthrough output is not redacted

#### Scenario: Help or completion is requested
- **WHEN** Cobra help or shell completion output is requested
- **THEN** it retains its native human or shell protocol and is not presented as a structured operation result

### Requirement: Operational resource targeting is consistent

Commands that inspect or execute against a running component SHALL accept either a workload identifier or a Onebox-run, Run-tier supporting-service identifier from the same project. Unknown targets SHALL return a typed error listing valid target identifiers and their target kinds.

#### Scenario: Run-tier service logs
- **WHEN** an operator requests logs for a declared Onebox-run PostgreSQL service
- **THEN** Onebox streams that service's container logs through the same output modes as workload logs

#### Scenario: Run-tier service exec
- **WHEN** an operator executes a command against a declared Onebox-run Redis service
- **THEN** Onebox runs it in the service container and records the target kind without adding its credentials to arguments, metadata, diagnostics, or envelopes

### Requirement: Benign no-ops are successful results

A command that establishes no change because desired and actual state already match SHALL return outcome `no_op`. Tool-specific benign no-change exit codes SHALL be normalized and SHALL NOT surface as product failures.

#### Scenario: Secret editor makes no change
- **WHEN** the trusted editor exits with the provider's documented no-change status
- **THEN** `secrets edit` returns outcome `no_op` with exit status 0

#### Scenario: Destructive confirmation is cancelled
- **WHEN** typed confirmation does not match
- **THEN** the command returns outcome `cancelled` with exit status 2 and performs no mutation

### Requirement: Local confirmation is not authenticated identity

`ob approve` SHALL create a short-lived local human confirmation bound to one exact executable plan. The artifact SHALL identify its source as `local_cli`, SHALL NOT be described as an authenticated identity-provider signature, and SHALL NOT be accepted as proof that the requesting actor was technically unable to mint it. Its integrity digest SHALL detect artifact modification but SHALL NOT be represented as issuer authentication.

#### Scenario: Agent inspects a local confirmation
- **WHEN** a local-confirmation artifact created by `ob approve` is returned in structured output
- **THEN** its source is explicitly `local_cli` and no field or documentation calls it independently authenticated

#### Scenario: Confirmation is changed after creation
- **WHEN** any plan binding, operator metadata, report binding, time, risk, or approval class in the confirmation changes
- **THEN** validation fails before target mutation

### Requirement: Backup reports are plan-bound execution inputs

When an executable plan requires migration backup protection, planning SHALL expose the exact protected resources, maximum report age, restore-test requirement, and required key-material names. `--backup-report-out` SHALL write a strict `onebox.run/backup-report/v1alpha1` template containing the exact plan digest and no backup bytes, secret values, commands, or provider credentials.

`ob approve --backup-report`, `ob deploy --backup-report`, and `ob job run --backup-report` SHALL accept the filled report. Confirmation SHALL bind its digest. Execution SHALL reject a missing, changed, stale, mismatched, incomplete, or invalid report before effects, then seal and journal an internal receipt for that execution attempt. Onebox SHALL describe the input as a report from an operator or external tool, not as proof that Onebox created or independently verified the backup.

#### Scenario: Plan requires a backup report
- **WHEN** a saved plan contains a migration backup requirement and `--backup-report-out` is supplied
- **THEN** planning writes a report template bound to that exact plan and returns its artifact path

#### Scenario: Report changes after confirmation
- **WHEN** a report's canonical digest differs from the digest bound into the supplied confirmation
- **THEN** execution fails before target mutation and directs the operator to confirm the current plan and report again

#### Scenario: Execution retries with the same report
- **WHEN** execution disconnects and the same plan, confirmation, and report are retried while still fresh
- **THEN** Onebox resumes the same operation identity and records one attempt-bound internal receipt without requiring a public receipt artifact

### Requirement: Safety flags grant one exact capability

Every safety-affecting flag SHALL map to one typed request field and one documented effect. A lock-breaking flag SHALL NOT permit mount detachment, version incompatibility, proxy ownership conflict, or migration-gate override. A mount-detachment flag SHALL NOT break a lock or permit an unsupported major-version transition. Unsupported transitions SHALL remain unavailable regardless of any override flag.

The execution boundary SHALL reject a request that supplies more than one executable plan, a plan for another operation kind, or a safety field that the selected operation does not consume. It SHALL NOT silently ignore irrelevant authority or safety inputs.

#### Scenario: Destructive mounts are allowed
- **WHEN** `service apply --allow-destructive-mounts` is invoked for a plan whose only blocked effect is an explicitly identified mount detachment
- **THEN** that detachment may proceed while lock ownership and version compatibility remain enforced

#### Scenario: Unsupported service major is requested with an override
- **WHEN** a service major-version transition is unsupported and any lock or mount override is supplied
- **THEN** execution fails before service mutation with the supported recovery or migration guidance

### Requirement: Secret sources are discoverable and individually editable

`ob secrets list` SHALL return every encrypted declaration with a stable value-free identifier, provider, source path, scope, order, output path, affected workloads, and editability without decrypting it. `ob secrets edit <entry-id>` SHALL edit exactly that SOPS source. Omitting the identifier SHALL succeed only when exactly one editable encrypted declaration exists; ambiguity SHALL fail with the list command as the next action.

#### Scenario: Project has multiple encrypted declarations
- **WHEN** `ob secrets edit` is invoked without an identifier
- **THEN** it changes nothing and returns a typed ambiguity error with `ob secrets list` as the next command

#### Scenario: Agent lists encrypted declarations
- **WHEN** `ob secrets list --output json` is invoked
- **THEN** the result contains stable identifiers and safe declaration metadata but no decrypted values or content hashes

### Requirement: Command guidance names its role

Typed errors SHALL distinguish a read-only `diagnostic_command`, a workflow-advancing `next_command`, and a mutation-capable `resolving_command`. `resolving_command` SHALL be omitted when the named command can only inspect or reproduce the condition.

#### Scenario: Status detects divergence
- **WHEN** `ob status` detects a condition it cannot repair
- **THEN** the error identifies any useful diagnostic or next command without naming `ob status` as resolving the divergence

### Requirement: Exec is an audited escape hatch

`ob exec` SHALL require a bounded single-line reason. It SHALL acquire the application host lock and mutation fence before resolving the target container, execute against that exact container, and append an audit record containing operator, target, target kind, resolved container identifier, command digest, reason, start time, and outcome. It SHALL NOT journal command bytes, stdin, stdout, or stderr. It SHALL continue to state that it is outside release convergence, rollback, idempotence, and passthrough redaction guarantees.

#### Scenario: Agent invokes exec without a reason
- **WHEN** `ob exec` is invoked without `--reason`
- **THEN** it runs no container command and returns a typed error naming the required flag

#### Scenario: Exec completes
- **WHEN** a reasoned exec command succeeds or fails
- **THEN** audit exposes the exact container and safe invocation evidence and never the command or passthrough bytes

#### Scenario: Exec races with a release change
- **WHEN** the target container would change while an exec is starting
- **THEN** lock and fence ownership make resolution and invocation one protected operation instead of running against an unjournaled replacement

### Requirement: The command/output matrix is closed

The CLI reference and tests SHALL enumerate every leaf command as one of: finite envelope, finite stream, unbounded stream, operator passthrough, trusted editor, help, or completion. Adding a command SHALL require selecting one class and proving its allowed output modes, terminal record, exit mapping, and stdout/stderr ownership.

#### Scenario: New leaf command is registered
- **WHEN** a new executable leaf command is added to the CLI
- **THEN** the command-matrix test fails until its output class and supported modes are declared
