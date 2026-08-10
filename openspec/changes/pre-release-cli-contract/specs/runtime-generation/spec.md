## ADDED Requirements

### Requirement: Every executable image is immutable in a sealed plan

Planning SHALL resolve every container image present in the bound application release runtime—including application, worker, daemon, job, and Compose-referenced workload images—to a digest reference. The plan SHALL bind every resolved reference, and the rendered runtime used by execution or by a future manual or scheduled job SHALL contain those digest references. A tag MAY be author input but SHALL NOT remain in an executable plan.

#### Scenario: Pre-release job uses a tag
- **WHEN** a project declares a job image by tag and a deploy plan is created
- **THEN** the job appears in `pinned_images` and the rendered job service uses the resolved digest reference

#### Scenario: Manual scheduled job uses a tag
- **WHEN** a manual scheduled job is included in the release runtime
- **THEN** its timer invokes a runtime containing the digest-pinned image even though the job is absent from deploy execution steps

#### Scenario: Any image cannot be resolved
- **WHEN** planning cannot resolve a runnable workload image to a digest
- **THEN** planning fails before approval with a typed error naming the workload and resolving command

#### Scenario: Compose-referenced workload uses a tagged image
- **WHEN** an adopted Compose service declares an image by tag
- **THEN** planning resolves and substitutes that service image while preserving every other authored Compose key

#### Scenario: Compose-referenced workload has only a build source
- **WHEN** an adopted Compose service has a build source and no resolved release image
- **THEN** planning fails before approval and names the image-resolution input the operator must provide

## MODIFIED Requirements

### Requirement: Service declarations produce no runtime and reserve their names

Application release generation SHALL NOT emit containers, volumes, or networks for a service declaration into the application's Compose runtime. A declared service backed by a shipped driver SHALL instead produce a separate, driver-owned runtime during `bootstrap` or `service apply`; it remains a Run-tier supporting service unless protection and restore evidence graduate it under a separate contract.

The names a service declaration derives SHALL be reserved across both the application and driver-owned runtimes. Preflight SHALL refuse a collision against them before either runtime mutates the target. Reserving a name SHALL NOT by itself create the resource, and application deployment SHALL NOT implicitly apply or upgrade the service runtime.

#### Scenario: Service declaration is inert during generation
- **WHEN** an application runtime is generated for a project declaring a shipped service driver
- **THEN** the application Compose contains nothing for that service and status identifies its separate Run-tier runtime

#### Scenario: Service is explicitly applied
- **WHEN** `bootstrap` or `service apply` executes for a declared shipped driver
- **THEN** Onebox generates and applies the separate driver-owned runtime without presenting backup or restore guarantees

#### Scenario: A service's future name is protected
- **WHEN** a foreign resource already holds a derived service name
- **THEN** preflight fails and names the collision before application or service mutation

#### Scenario: Workload depending on a declared service
- **WHEN** a workload references a declared service whose driver runtime is absent
- **THEN** application generation succeeds but preflight and status report the Run-tier service as not running

### Requirement: A target host has one application owner

The first-release host contract SHALL admit exactly one Onebox application identity. Host-scoped proxy, lock, registry, and layout state SHALL be owned by that identity and SHALL NOT merge routes or conflict policy across independent applications. A different application identity SHALL be refused before mutation. Destroy SHALL decide proxy removal from the one owner and SHALL NOT imply a supported multi-application registry.

#### Scenario: Another application targets an initialized host
- **WHEN** bootstrap, preflight, proxy apply, service apply, or deploy observes host state owned by a different application identity
- **THEN** it makes no mutation and returns a typed host-owner mismatch

#### Scenario: Owner destroys the application
- **WHEN** the sole application owner confirms destroy with proxy removal
- **THEN** Onebox does not consult or preserve a cross-application registry because no other application may be registered
