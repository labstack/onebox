## Why

Onebox today begins after the hard part. It requires a production Compose file, a
published image, a version scheme, and a provisioned host before it can do
anything, so every adopting project writes a wrapper to supply what Onebox will
not: none of the four `onebox.run/v1` projects in this organization can invoke
`ob` directly, and the most recently started project declined to adopt Onebox at
all in favor of a bespoke deploy script. Measured on a real adopting project,
about two thirds of the Compose file the user maintains is infrastructure that is
not their application — a data service, a cache, and proxy routing labels — and
the knowledge encoded there does not propagate: two projects by the same author
in the same month ship different and unequally correct data-service healthchecks.

`onebox.run/v1` is a classifier for a Compose file somebody else wrote. This
change makes the project file the authoring contract and makes the Compose
runtime a generated artifact, so that declaring an application is short, the
correct patterns are supplied rather than copied, and the runtime remains
inspectable and reclaimable.

## What Changes

- **BREAKING** Introduce `onebox.run/v2` as the authoring contract. v1 documented
  itself as evolving additively; v2 does not honor that promise because v1
  classifies Compose and v2 generates it. v1 remains loadable for one release
  cycle with a deprecation warning.
- Replace `components` with two blocks that state the ownership boundary in the
  shape of the file: `workloads` (containers the user owns, built from source or
  pulled by reference) and `services` (driver-backed supporting services).
  Third-party containers with no driver are workloads, not a third category.
- Accept a minimum project of an application identifier, a server, and one
  workload source. Every other field resolves from a documented default.
- Accept shorthand that expands deterministically into one canonical form.
  `postgres: 18` expands to a service declaration; top-level workload fields
  expand to a single workload named for the application.
- Generate the Compose runtime — workload services, networks, volumes, and proxy
  routes — from the normalized project, and pin what the plan will execute.
- Admit raw Compose per workload through a bounded reference to a named service
  in a user-authored file. Services accept no Compose escape hatch: a
  hand-authored data service is a workload and is not reported as managed.
- Make generation inspectable with a full rendered runtime, and reversible with
  an ejection that writes the generated runtime into the repository and
  permanently transfers ownership of it to the user.
- Keep CUE as the closed validation layer, reject unknown fields with correction
  hints, and export a JSON Schema per release so editors complete and document
  every field.
- Add conversion from a v1 project to v2 that reports what it could not convert
  rather than guessing.
- **BREAKING** Withdraw `ob.cue` as a second authoring format. YAML only.

### Non-Goals

This change establishes the contract and the generator. It does not:

- Implement any service driver, provision any service, or establish any service
  tier. `services` declarations validate and normalize; convergence, tiers,
  backups, and restore drills are separate changes and no service is reported as
  managed by this change.
- Move host provisioning into Onebox. Bootstrap remains as it is today.
- Change how versions are resolved, how images are built, or how releases are
  cut.
- Change approval, locking, fencing, journaling, drift, or rollback behavior.
- Retire the MCP adapter. It continues to serve v1 projects during the
  deprecation window and its withdrawal is a separate change.
- Delete or supersede the active `managed-service-operation-contract` change,
  which must be re-based on this contract before it is implemented.

## Capabilities

### New Capabilities

- `project-schema-v2`: The `onebox.run/v2` authoring contract — required and
  defaulted fields, the workload and service blocks, deterministic shorthand
  expansion to a canonical normalized form, closed validation with correction
  hints, the bounded per-workload Compose reference, and the exported JSON
  Schema.
- `runtime-generation`: Deterministic construction of the Compose runtime from a
  normalized v2 project — workloads, networks, volumes, proxy routes, and image
  references — together with full rendering for inspection and ejection that
  transfers ownership of the generated runtime to the repository.
- `project-migration-v1-to-v2`: Conversion of an `onebox.run/v1` project and its
  Compose file into a v2 project, the deprecation window during which v1 remains
  loadable, and honest reporting of constructs that cannot be converted.

### Modified Capabilities

None. `release-versioning` is unaffected: this change does not alter release
identity, build provenance, or runner compatibility ordering.

## Impact

- Authoring contract and validation: `internal/config/schema.cue`,
  `internal/config/cue.go`, `internal/config/config.go`, and the JSON Schema
  export added to the build.
- Runtime construction, classification, rendering, payload staging, and
  redaction: `internal/compose`.
- Normalization, plan inputs, and content commitments bound by the executable
  plan: `internal/onebox`.
- Release staging and rendered-runtime handling: `internal/engine`,
  `internal/release`.
- CLI surface for validation, configuration printing, rendering, ejection, and
  conversion: `cmd/ob`.
- MCP adapter reads the normalized model and must continue to serve v1 projects
  unchanged during the deprecation window: `internal/mcp`.
- Documentation: `docs/schema-v1.md` becomes the deprecated contract alongside a
  new v2 authoring guide; `README.md` and `docs/README.md` gain the v2 status
  and the deprecation window; `docs/product.md` states the ownership boundary as
  product direction.
- Compatibility: every existing v1 project keeps working for one release cycle.
  The four adopting projects in this organization are the conversion test suite,
  and the project that declined v1 is the acceptance test for expressiveness.
