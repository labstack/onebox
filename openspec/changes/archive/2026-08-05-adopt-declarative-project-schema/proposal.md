## Why

Onebox begins after the hard part. It requires a production Compose file, a
published image, a version scheme, and a provisioned host before it can do
anything, so every adopting project writes a wrapper to supply what Onebox will
not: none of the four projects in this organization can invoke `ob` directly,
and the most recently started project declined to adopt Onebox at all in favor
of a bespoke deploy script. Measured on a real adopting project, about two
thirds of the Compose file the user maintains is infrastructure that is not
their application, and the knowledge encoded there does not propagate: two
projects by the same author in the same month ship different and unequally
correct data-service healthchecks.

`onebox.run/v1` today is a classifier for a Compose file somebody else wrote.
This change redefines it as the authoring contract and makes the Compose runtime
a generated artifact, so that declaring an application is short, correct patterns
are supplied rather than copied, and the runtime remains inspectable and
reclaimable.

The schema is redefined in place rather than versioned forward. Onebox has no
published release, no tags, and no users outside this organization; the four
adopting projects are converted once, by hand. Carrying a second schema, a dual
loader, and a deprecation window would buy compatibility nobody is owed and would
double the surface under test.

Because the schema will carry real users after this change, its shape must be
right the first time. This proposal therefore treats coverage and evolution as
requirements, not as implementation detail.

## What Changes

- **BREAKING** Redefine `onebox.run/v1` as a declarative authoring contract. The
  previous meaning of that identity is withdrawn; projects using it are converted
  once. There is no deprecation window and no second loader.
- Replace `components` with two blocks that state the ownership boundary in the
  shape of the file: `workloads` (containers the user owns, built or pulled) and
  `services` (driver-backed supporting services). A third-party container with no
  driver is a workload, not a third category.
- Accept a minimum project of an application identifier, a server, and one
  workload source. Every other field resolves from a documented default whose
  origin is reported.
- Accept shorthand that expands deterministically into one canonical form.
- **Preserve every operational fact the previous schema could express.**
  Verification, lifecycle hooks, environment files and preflight checks,
  deployment order, release retention, migration policy, notifications, registry
  pull credentials, proxy configuration, and declared observability all carry
  forward. No shipped capability is dropped as a side effect of restructuring.
- **Admit growth without a second schema version.** Every field whose future
  needs are already visible accepts both a scalar shorthand and an object form;
  routing accepts multiple domains and non-HTTP entrypoints; environments may
  override a closed set of workload and service fields; `x-` keys are reserved
  for values Onebox ignores.
- Generate the Compose runtime — workload services, networks, volumes, and proxy
  routes — from the normalized project, and pin what the plan will execute.
- Admit raw Compose per workload through a bounded reference to a named service.
  Services accept no Compose escape hatch.
- Make generation inspectable with a full rendered runtime, and reversible with
  an ejection that permanently transfers ownership.
- Fix the remote layout and generated-resource naming as contract, including the
  volume names that cannot be renamed later without moving data, and make the
  base path a documented setting.
- Keep CUE as the closed validation layer, reject unknown fields with correction
  hints, and export a JSON Schema per release for editor completion.
- **BREAKING** Withdraw `ob.cue` as a second authoring format. YAML only.

### Non-Goals

This change establishes the contract, the generator, and the layout. It does not:

- Implement any service driver, provision any service, or establish any service
  tier. `services` declarations validate and normalize; convergence, tiers,
  backups, and restore drills are separate changes, and no service is reported as
  managed by this change.
- Move host provisioning into Onebox. Bootstrap remains as it is today.
- Change how versions are resolved, how images are built, or how releases are cut.
- Change approval, locking, fencing, journaling, drift, or rollback behavior.
- Retire the MCP adapter; its withdrawal is a separate change.
- Delete or supersede the active `managed-service-operation-contract` change,
  which must be re-based on this contract before it is implemented.
- Convert the four adopting projects automatically. Conversion is manual and is a
  task of this change, not a shipped capability.

## Capabilities

### New Capabilities

- `project-schema`: The `onebox.run/v1` authoring contract — required and
  defaulted fields, the workload and service blocks, deterministic shorthand
  expansion to a canonical normalized form, complete coverage of the operational
  facts the previous schema expressed, the documented evolution rules that let
  the contract grow additively, environment overrides, reserved extension keys,
  closed validation with correction hints, and the exported JSON Schema.
- `runtime-generation`: Deterministic construction of the Compose runtime from a
  normalized project — workloads, networks, volumes, routing, and image
  references — the fixed remote layout and naming contract, full rendering for
  inspection, and ejection that transfers ownership to the repository.

### Modified Capabilities

None. `release-versioning` is unaffected: this change does not alter release
identity, build provenance, or runner compatibility ordering.

## Impact

- Authoring contract and validation: `internal/config/schema.cue`,
  `internal/config/cue.go`, `internal/config/config.go`, and the JSON Schema
  export added to the build.
- Runtime construction, classification, rendering, payload staging, and
  redaction: `internal/compose`.
- Normalization, plan inputs, and content commitments: `internal/onebox`.
- Remote layout, naming, and release staging: `internal/engine`,
  `internal/release`, `internal/proxy`.
- CLI surface for validation, configuration printing, rendering, and ejection:
  `cmd/ob`.
- MCP adapter reads the normalized model and must keep working against the
  redefined contract: `internal/mcp`.
- Documentation: `docs/schema-v1.md` is rewritten for the declarative contract;
  `README.md` and `docs/README.md` gain the breaking-change notice;
  `docs/product.md` states the ownership boundary as product direction.
- Compatibility: **every existing project file stops loading and is converted
  once.** The four adopting projects are the conversion suite, and the project
  that declined the previous schema is the acceptance test for expressiveness.
