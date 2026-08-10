## MODIFIED Requirements

### Requirement: Workloads may publish host ports

A workload MAY publish a host port, for a service reached without going through the proxy. A published port SHALL declare the host port, the container port, a bind address defaulting to `127.0.0.1`, and a protocol of `tcp` or `udp` defaulting to `tcp`. A workload MAY publish the same port under both protocols. Publishing on every interface SHALL require declaring the bind address explicitly, so exposure is deliberate rather than accidental.

A workload using the rolling deployment strategy SHALL NOT publish a fixed host port, because the replacement and serving replicas must coexist during the ready gate. This incompatibility SHALL be rejected during project validation, before planning or target contact, and the error SHALL direct the author to remove the published port or select `recreate`.

#### Scenario: Default bind is loopback
- **WHEN** a workload publishes a port without a bind address
- **THEN** the canonical form binds it to `127.0.0.1`

#### Scenario: UDP port
- **WHEN** a workload publishes the same port over TCP and UDP
- **THEN** both are declared and both appear in the generated runtime

#### Scenario: Public exposure is explicit
- **WHEN** a workload publishes a port on every interface
- **THEN** the bind address is stated in the project file

#### Scenario: Rolling workload publishes a fixed port
- **WHEN** a rolling workload declares any published host port
- **THEN** validation fails before target contact and names both compatible resolutions

## ADDED Requirements

### Requirement: Manual jobs never enter automatic deployment

A job with `when: manual` SHALL run only through the canonical one-shot job operation or its declared host schedule. It SHALL NOT appear in a deploy operation graph and SHALL NOT execute as a pre-release or post-release step. Scheduled execution does not change its manual deployment semantics, and an unscheduled manual job remains reachable through `ob job plan|run`.

#### Scenario: Manual scheduled job is deployed
- **WHEN** a job declares both `when: manual` and a schedule
- **THEN** deployment installs or updates its timer without executing the job during that deployment

#### Scenario: Manual job appears in a sealed plan
- **WHEN** a deploy plan is created for a project containing a manual job
- **THEN** the job is absent from automatic execution steps and remains available to the one-shot job operation and any declared schedule

#### Scenario: Unscheduled manual job
- **WHEN** a manual job declares no schedule
- **THEN** the project remains valid and the job is invokable through `ob job plan|run`
