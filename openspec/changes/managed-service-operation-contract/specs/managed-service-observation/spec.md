## Purpose

Defines complete, redaction-safe managed-service state reporting that distinguishes desired configuration, recorded application, actual runtime state, and unavailable evidence.

## ADDED Requirements

### Requirement: Observation separates desired, applied, and actual state
For each managed component, observation SHALL report the desired payload digest, recorded applied digest, actual running image digest, service version, driver contract, profile, health, container state, stable network identity, persistent-volume identities, captured time, drift, completeness, and bounded issues. It SHALL not infer that matching files imply matching runtime state.

#### Scenario: Service is fully in sync
- **WHEN** desired, applied, and actual state match and driver health checks succeed
- **THEN** observation reports the component complete, healthy, and not diverged

#### Scenario: Configuration file changed but service did not converge
- **WHEN** desired or live configuration differs from the applied marker or running service state
- **THEN** observation reports configuration drift and does not claim the new configuration is active

#### Scenario: Container is absent
- **WHEN** a managed component is configured but has no running container
- **THEN** observation reports it absent and diverged with a managed-service apply instruction

### Requirement: Observation is honest about incomplete evidence
Every observation source SHALL distinguish positive absence from read failure. If a required source is unavailable, unreadable, malformed, or contradictory, the component and aggregate observation SHALL be incomplete and SHALL include a stable warning; they SHALL NOT substitute healthy, empty, or in-sync state.

#### Scenario: Volume inspection fails
- **WHEN** container health is readable but required volume identity cannot be inspected
- **THEN** observation preserves the known health fact, marks the component incomplete, and reports the missing volume evidence

#### Scenario: Applied state is unreadable
- **WHEN** the applied marker path exists but cannot be read
- **THEN** observation reports an applied-state read failure rather than treating the service as never applied

### Requirement: Effective settings and origins are inspectable
Observation and resolved-configuration output SHALL expose all non-sensitive Onebox-controlled effective settings with origin, profile identifier, and change classification. Driver-readable upstream runtime settings SHALL be returned only from an explicit bounded allowlist and SHALL be marked as observed facts rather than desired configuration.

#### Scenario: Profile default is active
- **WHEN** a profile supplies a resource or service setting that the user did not override
- **THEN** resolved output shows the value, origin `profile`, and the immutable profile identifier

#### Scenario: Native runtime value is observed
- **WHEN** a driver safely reads an allowlisted native setting delegated to the upstream image
- **THEN** observation marks the value as `observed upstream` and does not rewrite project configuration

### Requirement: Observation never exposes credentials or unbounded service output
Observation SHALL exclude plaintext secrets, environment values, authentication material, private keys, raw configuration files, raw service logs, and arbitrary service error bodies. Secret mappings SHALL be represented only by logical slot and source-presence facts. All arrays, strings, and diagnostics SHALL have deterministic ordering and explicit size limits.

#### Scenario: Secret is configured
- **WHEN** a managed component has a valid password mapping
- **THEN** observation reports that the required logical slot is present without returning the encrypted key name when policy hides it or any plaintext value

#### Scenario: Service returns a long error
- **WHEN** a health or version probe returns untrusted or oversized output
- **THEN** observation returns a stable bounded issue code and safe summary rather than the raw output

### Requirement: Read-only observation performs no convergence
Status and MCP observation SHALL not create networks or volumes, pull images, render secret files, restart containers, repair configuration, update applied markers, or otherwise mutate local or remote service state.

#### Scenario: Drift is observed
- **WHEN** a read-only observation finds a missing network or unhealthy managed service
- **THEN** it reports the condition and next planning action without attempting repair
