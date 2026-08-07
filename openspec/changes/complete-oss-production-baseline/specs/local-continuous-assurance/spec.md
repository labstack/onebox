## Purpose

Provides agentless, continuously scheduled evidence that the local production host remains healthy, protected, and reachable between explicit Onebox invocations.

## ADDED Requirements

### Requirement: Assurance checks are read-only and host-native

Onebox SHALL install host timers that evaluate workload and service health,
disk pressure, certificate runway, backup freshness, restore-drill freshness,
and generated-unit state without a resident Onebox process. A check SHALL NOT
restart, repair, prune, deploy, renew, or otherwise converge the target.
For a `disable-pending` service, checks SHALL continue to observe service health,
required unit safety, state age, deadline, and continued storage activity, but
SHALL exclude backup and restore-drill freshness from health transitions because
those no longer determine the service tier.

#### Scenario: Unhealthy workload
- **GIVEN** a workload becomes unhealthy between deployments
- **WHEN** the assurance timer runs
- **THEN** it records the observed failure and does not restart the workload

#### Scenario: Check is cancelled
- **GIVEN** an assurance check is interrupted
- **WHEN** its next schedule runs
- **THEN** it performs a fresh read and does not infer success from incomplete evidence

#### Scenario: Protection disablement is pending
- **GIVEN** a service deliberately entered `disable-pending`
- **WHEN** assurance runs after backup or restore proof would otherwise become stale
- **THEN** it reports `protection_disable_pending` with age, deadline, continued storage activity, and the exact resolving command instead of emitting backup- or drill-freshness failures

### Requirement: Notifications describe transitions without leaking secrets

The watchdog SHALL use the existing notification destination and outcome
filter, emit on state transitions and bounded reminders, and include stable
component and evidence identifiers. Notification bodies and delivery errors
SHALL exclude secret values, credential-bearing URLs, private certificate
material, and database content. Notification failure SHALL NOT alter the
observed health result.
Entering `disable-pending` SHALL emit one state transition. Freshness expiry
while that state persists SHALL emit no backup or drill transition; crossing the
action deadline SHALL instead emit `protection_disablement_overdue` and use the
bounded reminder cadence for that actionable state.

#### Scenario: Healthy service becomes unhealthy
- **GIVEN** the previous completed check recorded the service healthy
- **WHEN** a completed check records it unhealthy
- **THEN** one transition notification is attempted and the failure remains recorded even if delivery fails

#### Scenario: Repeated unchanged failure
- **GIVEN** the same failure persists across checks
- **WHEN** no reminder interval has elapsed
- **THEN** no duplicate notification is emitted

#### Scenario: Pending disablement proof expires
- **GIVEN** a service remains `disable-pending` after its prior backup or restore-drill proof expires
- **WHEN** the watchdog compares the completed check with prior evidence
- **THEN** it emits no stale-backup or stale-drill transition and keeps the pending disablement as the only protection action state

### Requirement: Assurance evidence is honest about staleness

Each result SHALL carry its observation time, runner provenance, check set,
completion state, and expiry. Status and doctor SHALL distinguish current,
stale, incomplete, never-run, and failed evidence; none may be rendered as a
passing current check.

#### Scenario: Host timer has stopped
- **GIVEN** the last completed evidence exceeds its expiry
- **WHEN** status is requested
- **THEN** output reports `assurance_stale` and names the timer-inspection command

#### Scenario: Partial check set
- **GIVEN** certificate inspection completed but backup inspection failed
- **WHEN** evidence is rendered
- **THEN** the completed and incomplete checks remain distinct and overall assurance does not pass

### Requirement: Assurance status is agent-operable

Assurance inspect and status commands SHALL return versioned structured output
with stable check codes, timestamps, evidence identifiers, and resolving next
commands. Repeated reads SHALL be side-effect free, and a runner disconnection
SHALL not cause a later reader to treat an incomplete record as terminal proof.

#### Scenario: Agent reads current assurance
- **GIVEN** a completed assurance record exists
- **WHEN** JSON output is requested
- **THEN** the response carries its schema version, per-check state, overall state, expiry, and safe next commands
