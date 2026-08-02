## Purpose

Defines the LLM-first MCP interface through which agents discover managed-service capabilities, propose safe changes, obtain trusted approval, execute operations, and verify outcomes.

## ADDED Requirements

### Requirement: Agents can discover supported service contracts
The MCP server SHALL expose a read-only, schema-validated service catalog containing supported component types, driver contract identifiers, profiles, image constraints, typed settings, bounded native-parameter rules, secret slots, effective defaults, validation constraints, change classifications, deprecations, and runner compatibility. Catalog responses SHALL be deterministic, versioned, bounded, and pageable when necessary.

#### Scenario: Agent explores PostgreSQL support
- **WHEN** an agent requests the catalog entry for a supported PostgreSQL driver contract
- **THEN** the tool returns structured setting schemas, profiles, defaults, origins, secret slots, effect classifications, and examples without requiring the model to inspect source code or prose documentation

#### Scenario: Runner lacks requested contract
- **WHEN** an agent requests a contract unsupported by the selected runner
- **THEN** the tool returns a stable unsupported-contract error with available identifiers and upgrade guidance

#### Scenario: Catalog is too large for one result
- **WHEN** a catalog response exceeds its configured item or byte limit
- **THEN** the tool returns a bounded page and opaque continuation cursor without truncating a setting definition mid-object

### Requirement: Agents propose changes as structured intent
The MCP server SHALL expose a read-only proposal tool that accepts a typed component target, desired driver/profile/image, structured settings patch, secret-slot references, and an idempotency key. It SHALL validate the intent, observe the target, and return a sealed redaction-safe proposal or structured blockers. It SHALL NOT accept arbitrary shell commands, raw Compose, plaintext credentials, or an unconstrained YAML document as managed-service intent.

#### Scenario: Agent proposes a valid setting change
- **WHEN** an agent submits a valid typed settings patch for one managed component
- **THEN** the tool returns one immutable proposal containing resolved settings, origins, desired-versus-observed changes, risk, reversibility, interruption, verification, approval requirement, expiry, and stable proposal identifier

#### Scenario: Agent retries the same proposal request
- **WHEN** the same authorized caller repeats an identical request with the same idempotency key
- **THEN** the server returns the same proposal identity or an explicit superseded-state result rather than creating ambiguous duplicates

#### Scenario: Intent contains an invalid field
- **WHEN** a settings patch contains an unknown, mistyped, protected, or type-invalid field
- **THEN** the tool returns a stable validation error containing the field path, expected schema, safe correction guidance, and no arbitrary parser output

### Requirement: Agents persist desired state without authoring YAML
When accepted structured intent differs from project configuration, the MCP server SHALL return a closed project change bound to the current project-file revision and SHALL withhold an executable runtime plan until that desired state is persisted. A local project-change tool SHALL apply only the exact proposed semantic change to the configured project file, preserve unrelated content, validate the result, use atomic replacement, and never connect to or mutate the production target. It SHALL accept no arbitrary path, document, YAML, or shell input.

#### Scenario: Proposed setting is not yet in the project
- **WHEN** an agent proposes a valid managed setting that differs from durable project configuration
- **THEN** the proposal returns a revision-bound project change and `runtime_plan_ready: false` instead of planning production from transient conversation

#### Scenario: Agent applies the exact project change
- **WHEN** the authorized agent submits the unexpired project-change identifier, matching base revision, and idempotency key
- **THEN** the server atomically applies and validates the exact change locally, returns the new project revision, and performs no target connection

#### Scenario: Project changed after proposal
- **WHEN** the project-file revision no longer matches the proposed change
- **THEN** the server refuses the stale local mutation without partially editing the file and directs the agent to propose again

#### Scenario: Agent proposes after persistence
- **WHEN** the project contains the accepted desired state and the agent proposes the service change again
- **THEN** the server may observe production and create a state-bound runtime proposal from that durable configuration

### Requirement: Model intent is not trusted approval
A model statement, tool-call argument, conversation message, or form-mode elicitation response SHALL NOT satisfy an operation approval. Required approval SHALL be issued by a trusted identity and bound to the exact proposal, target, effects, expiry, and approval class. Sensitive credentials SHALL be collected only through an out-of-band trusted surface and SHALL never transit model context.

#### Scenario: Agent claims the user approved
- **WHEN** an execution request includes text asserting approval but no valid bound approval capability
- **THEN** execution refuses before target mutation and returns the required approval state

#### Scenario: Proposal requires human approval
- **WHEN** a proposal's policy or risk requires approval
- **THEN** the MCP result returns a safe approval handoff and pending state that the client can present without embedding a pre-authenticated credential in model-visible content

#### Scenario: User supplies a secret
- **WHEN** a required secret does not yet exist
- **THEN** the server directs the user to a trusted out-of-band secret-entry flow and does not request the secret through ordinary tool arguments or form-mode elicitation

### Requirement: Approved execution is asynchronous and idempotent
The MCP server SHALL expose approved operation execution as a durable operation or task with a stable identifier, terminal state, ordered progress, cancellation semantics, and idempotent retry behavior. The initial call SHALL return promptly once execution is accepted. Callers SHALL be able to retrieve current state and terminal redaction-safe evidence without depending on notifications.

#### Scenario: Approved operation starts
- **WHEN** a valid proposal and matching approval capability are submitted for execution
- **THEN** the server returns a stable operation identifier and accepted state before waiting for long-running convergence

#### Scenario: Agent polls operation state
- **WHEN** an agent requests an accepted operation by identifier
- **THEN** the server returns ordered bounded progress, current phase, cancellation availability, and terminal evidence when complete

#### Scenario: Notification is missed
- **WHEN** the client does not receive or retain progress notifications
- **THEN** polling the operation identifier still returns authoritative current and terminal state

#### Scenario: Execution request is retried
- **WHEN** the same caller retries execution for the same proposal and approval after acceptance
- **THEN** the server returns the existing operation instead of starting a second mutation

#### Scenario: Cancellation is requested
- **WHEN** an authorized caller cancels a cancellable operation
- **THEN** the operation transitions to cancelled, execution attempts bounded cooperative stop, and subsequent observation reports actual target state without rewriting the terminal task status

### Requirement: Tool schemas and annotations describe real behavior
Every managed-service MCP tool SHALL declare strict input and output schemas and accurate read-only, destructive, idempotent, and open-world annotations. Successful structured content SHALL conform to its output schema. The server SHALL validate both tool input and generated output and SHALL fail closed on schema mismatch.

#### Scenario: Read-only catalog tool is listed
- **WHEN** a client inspects the catalog tool definition
- **THEN** it is annotated read-only and non-destructive and its output schema describes the complete structured result

#### Scenario: Execution tool is listed
- **WHEN** a client inspects the approved execution tool definition
- **THEN** it is annotated as mutating with idempotency semantics that match server behavior and does not claim to be read-only

#### Scenario: Server output violates its schema
- **WHEN** an internal defect produces a response that does not conform to the declared output schema
- **THEN** the server returns a stable internal-contract error instead of sending malformed structured content to the model

### Requirement: Agent-visible errors are typed and actionable
Tool errors SHALL use stable codes and structured fields for phase, component, field path, retryability, required action, safe summary, and correlation identifier. Model-facing errors SHALL NOT contain raw remote stderr, service logs, configuration bodies, secret names prohibited by policy, stack traces, or unbounded text.

#### Scenario: Target is temporarily unavailable
- **WHEN** observation or execution fails because the target cannot be reached
- **THEN** the tool returns a retryable connectivity code, bounded safe summary, correlation identifier, and appropriate next action

#### Scenario: Safety gate refuses a change
- **WHEN** a requested action violates a persistent-data or upgrade gate
- **THEN** the tool returns a non-retryable-without-change safety code and a structured alternative operation or remediation requirement

### Requirement: LLM context is minimized by default
MCP results SHALL return only the fields needed for the current decision, use stable summaries plus opaque identifiers for larger artifacts, and bound item counts and text sizes. Raw rendered Compose, full configuration, journals, and logs SHALL be available only through explicit, separately authorized, redaction-safe resources or tools and SHALL not be embedded automatically in every response.

#### Scenario: Proposal has a large rendered diff
- **WHEN** the complete redaction-safe diff exceeds the proposal response limit
- **THEN** the response includes a bounded semantic summary and opaque resource reference rather than truncating or flooding model context

#### Scenario: Agent requests final verification
- **WHEN** an operation reaches a terminal state
- **THEN** the operation result includes the bounded verification facts and an observation identifier needed to confirm outcome, not the entire execution journal

### Requirement: CLI remains a non-primary adapter
Any CLI support for managed services SHALL call the same canonical operations service, schemas, authorization checks, plan validation, and execution boundary used by MCP. The CLI SHALL contain no unique managed-service lifecycle or safety behavior and SHALL be documented as test, support, and break-glass access rather than the normal product workflow.

#### Scenario: CLI executes an approved operation
- **WHEN** support tooling invokes a managed-service operation through the CLI
- **THEN** the same proposal, approval, binding, execution, event, and evidence rules apply as through MCP
