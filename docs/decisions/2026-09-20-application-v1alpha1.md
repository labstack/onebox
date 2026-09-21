# Application contract reset to `onebox.run/v1alpha1`

- Status: accepted
- Date: 2026-09-20
- Tracking issue: [#202](https://github.com/labstack/onebox/issues/202)
- Scope: M0 — contract decision, naming specification, and cutover inventory

## Context

The current authored project file uses `onebox.run/v1`, a flat root object,
snake_case fields, and an optional single-workload shorthand. That identity was
assigned before Onebox had a governed public baseline. Keeping it would turn a
development shape into a compatibility promise and would preserve two authored
representations for the same workload.

Onebox also writes machine artifacts such as plans, confirmations, results, and
host state. Those artifacts have independent identities and recovery rules.
Changing the human-authored resource must not silently rename their fields or
advance their schemas.

## Decision

The only authored Application identity is `onebox.run/v1alpha1`:

```yaml
apiVersion: onebox.run/v1alpha1
kind: Application
metadata:
  name: shop
spec:
  environments: {}
  workloads: {}
```

The root contains exactly `apiVersion`, `kind`, `metadata`, and `spec`.
`apiVersion` and `kind` are the resource's type identity. `metadata.name`
replaces `app`. All Onebox configuration is below `spec`; observed state is not
part of this authored resource. A future observed resource may add a top-level
`status`, but this Application does not accept one.

Every application uses `spec.workloads`. The root-level `build`, `image`,
`compose`, `port`, `health`, and `routes` shorthand is removed. Defaults may
keep small resources concise, but the parser exposes only one authored shape
for a workload.

The public schema uses JSON Schema Draft 2020-12, lives at
`api/application/v1alpha1/application.schema.json`, and has the stable ID
`https://onebox.run/schemas/application/v1alpha1/application.schema.json`.
The binary embeds that schema. The checked-in schema remains reviewable, and CI
proves byte-for-byte agreement between it, the Go model, and the schema emitted
by the binary. Cross-field and operational rules remain in the semantic
validator.

## Naming specification

Onebox has two deliberately separate wire dialects.

### Authored declarative resources

- Fields use lowerCamelCase: `apiVersion`, `basePath`, `backupTargets`,
  `externalServices`, `envFiles`, `dataEffect`, and `deploymentPhase`.
- Resource kinds and finite declarative values use singular UpperCamelCase:
  `Application`, `Job`, `PreRelease`, and `ClientSide`.
- Lowercase values remain lowercase only when they are data rather than Onebox
  constants, including identifiers, protocols, image references, hostnames,
  paths, and provider-native values.
- Go identifiers use the standard initialisms `ID`, `URL`, `HTTP`, `JSON`,
  `SSH`, `UID`, and `GID`.
- References are typed objects named `<subject>Ref` and contain at least
  `name`. Timestamps are named `<subject>Time` and serialize as RFC 3339.
- Durations and quantities use standard string syntax unless the unit is part
  of the field name.
- Map keys identify declared objects; they do not extend behavior.
- Unknown and duplicate fields are rejected at every typed level.
- Behavior-bearing `x-*` fields are rejected. Opaque user metadata may be
  stored in `metadata.annotations`, and annotations must not affect generated
  plans or runtime behavior.

### Machine artifacts

- Artifact identity remains in `schema_version`.
- Machine fields remain snake_case and machine codes remain lower_snake_case.
- Existing artifact identities advance only through their own schema change,
  compatibility decision, and recovery assessment.
- The Application reset does not rename plan, confirmation, result, release,
  host-state, or structured-output fields.

This boundary is mechanical: declarative resource checks must not be applied to
machine artifacts, and machine-field checks must not permit snake_case in an
authored Application.

## Version and compatibility policy

This is an intentional breaking reset before the first public baseline is
locked. The active product contract does not retain or translate
`onebox.run/v1` and does not accept aliases for its keys or enum spellings.
There is no legacy loader, dual read or write, migration command, automatic
rewrite, fallback, or compatibility fixture that promises v1 remains valid.

Old files fail with a concise diagnostic that names
`onebox.run/v1alpha1` and points to the current field reference. Users rewrite
their Application explicitly.

The raw authored project bytes participate in `config_digest`. Rewriting a
project therefore invalidates every outstanding plan and local confirmation
bound to the old bytes. They must be regenerated after the rewrite. Durable
release manifests and host state keep their independent schema identities;
they are not made current by accepting stale plan input or by relabeling their
wire format.

Alpha does not mean implicit compatibility. Any incompatible change after this
baseline advances the authored API identity and states its migration policy.
The first stable compatibility guarantee begins only when a stable API is
declared.

## Product boundary

Onebox and Multibox share the 2026 vocabulary and casing conventions, but they
are independent products. Onebox owns its Application schema, loader, runtime,
release policy, environments, host routing, backups, and deployment behavior.
There is no cross-repository schema import or runtime dependency. A convention
change may be coordinated, but each product versions, reviews, verifies, and
releases its own contract.

## Cutover inventory

This inventory freezes the M0 audit boundary. Generated artifacts are not
edited by hand; their generators change in the milestone named below. Authored
documentation continues to describe the shipped v1 binary until the code
cutover is complete, then changes atomically in M5.

### Generated contract artifacts and reference pages

| Current path | Owner | Cutover |
| --- | --- | --- |
| `docs/onebox.run-v1.schema.json` | `internal/app/jsonschema.go`, `ob schema` | M1 introduces `api/application/v1alpha1/application.schema.json`; M5 removes the retired publication path. |
| `site/public/onebox.run-v1.schema.json` | `cmd/ob-docgen` | M5 publishes the v1alpha1 schema at the website path matching the stable schema ID. |
| `site/src/content/docs/reference/fields/*.mdx` | `cmd/ob-docgen` | M5 regenerates field names, nesting, requiredness, values, and examples. |
| `site/src/content/docs/reference/cli.mdx` | Cobra help through `cmd/ob-docgen` | M3 changes command help; M5 regenerates the page. |
| `site/src/content/docs/reference/errors.mdx` | loader and lifecycle registries through `cmd/ob-docgen` | M3 adds the cutover diagnostics; M5 regenerates the page. |
| `site/src/content/docs/reference/drivers.mdx` | service-driver catalogue through `cmd/ob-docgen` | M5 regeneration verifies the generator remains complete even when its prose is unchanged. |

The generator entry points that carry old names or destinations are
`cmd/ob-docgen/main.go`, `internal/app/jsonschema.go`, `cmd/ob/schema.go`, and
`cmd/ob/init.go`. CI references in `.github/workflows/ci.yml`, the `justfile`
documentation targets, and `cmd/ob-docgen/main_test.go` must follow the new
schema path and continue enforcing exact agreement.

### Authored user documentation

The following authored surfaces contain the old identity, old shape, old field
names, old enum spellings, or examples whose indentation changes under `spec`:

- `README.md` and `docs/README.md`;
- `site/src/components/landing/Derivation.astro` and
  `site/src/components/landing/Hero.astro`;
- `site/src/content/docs/reference/project-file.mdx`, `naming.mdx`,
  `policies.mdx`, and `status/capabilities.mdx`;
- `site/src/content/docs/start/reading-it-back.mdx`;
- `site/src/content/docs/explanation/what-onebox-refuses.mdx`;
- the guides `adopt-compose.mdx`, `back-up-a-database.mdx`,
  `environment-variables.mdx`, `handle-secrets.mdx`, `roll-back.mdx`,
  `run-migrations.mdx`, and `schedule-a-job.mdx`;
- authored text surrounding generated content in
  `site/src/content/docs/reference/cli.mdx` and `errors.mdx`;
- the source diagrams `docs/media/deploy-light.svg` and
  `docs/media/deploy-dark.svg`.

M5 updates these only after the loader, runtime consumers, CLI, examples, and
end-to-end corpus all use v1alpha1. Historical decision records remain
historical and are not rewritten to pretend they described the new API.

### Repository-owned Applications and conformance data

M4 rewrites every shipped Application example or fixture:

- all `e2e/apps/*.yml` files and `e2e/apps/README.md`;
- `e2e/testdata/app/ob.yml`, `e2e/testdata/postgres/ob.yml.tmpl`, and
  `e2e/testdata/worker/{ob.yml,ob-broken.yml}`;
- all authored YAML under `internal/app/testdata/corpus/` and its `README.md`;
- `internal/app/testdata/contract-verdicts.json` and every inline project
  fixture in `internal/app`, `internal/engine`, `internal/onebox`,
  `internal/proxy`, and `cmd/ob` tests.

The new schema-owned fixture corpus is rooted at
`api/testdata/application/{valid,invalid}/`. It must cover the envelope,
identity, metadata, strict field closure, lowerCamelCase names, UpperCamelCase
values, every supported workload/source form, and explicit rejection of v1,
`api_version`, root `app`, snake_case declarative fields, shorthand, aliases,
and behavior-bearing extensions.

## Delivery and verification

The cutover implements M0 through M6 from issue #202 atomically in one pull
request. This is an explicit delivery override: splitting a breaking wire
reset across mergeable intermediate states would leave the loader, generated
schema, examples, and documentation describing different contracts. The PR
keeps milestone-specific commits and verification evidence where useful, while
the merge boundary remains the complete cutover.

Final acceptance additionally requires `just ci` and the applicable Docker E2E
suites. M3 proves stale plan and confirmation rejection after project bytes
change. M6 mechanically prevents authored and machine dialects from drifting
together.

## Consequences

- Existing v1 project files stop loading and require an explicit rewrite.
- Outstanding plans and confirmations become stale when that rewrite changes
  the bound project bytes.
- The public authored contract gains a conventional resource envelope and one
  canonical workload shape.
- Runtime internals may retain implementation-specific names, but no old wire
  spelling is accepted or emitted at the authored boundary.
- Machine artifacts remain independently versioned and recoverable according
  to their own contracts.
