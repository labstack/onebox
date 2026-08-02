## Why

Container names are host-global in the container runtime, but Onebox derives
them from the component name alone. `slotNames` in `internal/engine/roll.go:241`
returns the bare service name for a single replica and `<service>-<n>` beyond
that, and `roll.go:215` applies the result with `docker rename`.

Every project in this organization names its application component `server`, so
any two of them deployed to one host collide: the second `docker rename` fails
against a name the first already holds, mid-rollout, after new containers have
started. It has not been hit because each application currently has its own
host — but the product explicitly supports several applications sharing one, so
this is a live defect waiting on the first person to take that offer.

This is separated from the declarative-schema work deliberately. That change is
large and unimplemented; this is a small correction to shipped behavior that
should not wait for it.

## What Changes

- Derive container names from the application and the component together rather
  than the component alone, so two applications on one host cannot collide.
- **BREAKING** for running installations: the next deployment renames containers.
  Because renaming is what the rollout already does, and the release directory,
  volumes, networks, and Compose project are unaffected, this is visible in
  `docker ps` output and in nothing else. Rollback to a previous release
  re-derives names the same way and is unaffected.
- Refuse a derived container name exceeding the container runtime's limit at
  validation, naming the identifiers and the limit, rather than truncating into
  a possible collision.
- Detect and report a pre-existing foreign container holding a derived name,
  rather than failing partway through the rollout.

### Non-Goals

- No change to the Compose project name, network names, volume names, release
  layout, or any other derived identifier. Those are addressed by the
  declarative-schema change and are not host-global in the same way.
- No change to rollout, drain, health-gating, or rollback behavior.

## Capabilities

### New Capabilities

- `container-naming`: Host-unique derivation of container names from the
  application and component, refusal rather than truncation at the runtime's
  length limit, and detection of a foreign holder before mutation.

### Modified Capabilities

None. `release-versioning` is unaffected.

## Impact

- Name derivation and rollout renaming: `internal/engine/roll.go`.
- Status and observation surfaces that match containers by name:
  `internal/engine/status.go`, `internal/engine/status_snapshot.go`.
- Any test asserting a bare component name as a container name.
- Operational note: the first deployment after this ships renames the running
  containers of every existing installation.
