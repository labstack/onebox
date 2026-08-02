## Context

See proposal.md — Why.

The current loader treats the Compose file as the source of truth and the
project file as an annotation over it: `compose.Load` reads the user's Compose
project, `compose.Classify` attaches a v1 component type to each service, and
`internal/engine` renders a per-release Compose file by injecting a bounded
delta — release identity, network attachment, and rollout naming — into what the
user wrote. Validation is layered: `internal/config/schema.cue` owns shape,
enums, and scalar patterns; `config.Validate` owns cross-field rules;
`compose.Classify` owns Compose-semantic rules.

That injection is the seed of this change. Onebox already generates part of the
runtime and already proves it can do so deterministically and reversibly. v2
widens the generated portion from a delta to the whole file while keeping the
same properties.

Two constraints shape the approach. Existing v1 projects must keep deploying
unchanged for a release cycle, so the engine cannot be rewritten around a v2-only
model. And the sealed-plan contract binds exact content, so whatever generation
produces has to be reproducible from the plan's own inputs at execution time.

Diagrams below are D2 source, validated against d2 0.7.1.

## Goals / Non-Goals

**Goals:**

- One internal normalized model that both the v1 and v2 loaders produce, so the
  engine, planner, and approval paths are unchanged by this contract.
- Generation that is deterministic, digest-bindable, fully inspectable, and
  reversible.
- A merge boundary with Compose-authored workloads that is enumerated rather
  than discretionary, so a user can predict exactly what Onebox will touch.
- Defaults that resolve from a documented precedence chain and report their
  origin, so a generated value is never mistaken for a stated intent.

**Non-Goals:**

- No driver, convergence, tuning, backup, or tier behavior for services. This
  design normalizes service declarations and stops.
- No change to lock, fence, journal, drift, approval, verification, or rollback
  mechanics.
- No change to how images are built or how versions are cut. Build-sourced
  workloads depend on an image reference resolved by the mechanism that exists
  today until the release-pipeline change lands.

## Decisions

### The ownership boundary is expressed as two blocks, not three

```d2
direction: down

user: User declares {
  style.fill: "#eef6ff"
  wl: "workloads\nbuild: | image: | compose:"
  sv: "services\ndriver + version + settings"
}

onebox: Onebox derives {
  style.fill: "#f5f5f5"
  net: networks
  vol: volumes
  route: proxy routes
  rel: release identity
}

runtime: "generated runtime (digest-bound)" {
  style.fill: "#e8f5e9"
}

user.wl -> runtime
user.sv -> runtime: "inert in this change"
onebox -> runtime
```

A container that is neither built by the user nor backed by a driver — searxng,
clamav, ofelia, mailpit — is a workload sourced by image reference. The
alternative, a third category for third-party containers, was rejected: it
duplicates every workload field, and the distinction it encodes (whose source
code is it) has no operational consequence. What has consequence is whether a
driver owns the lifecycle, and that is exactly the workload/service split.

### CUE stays; a JSON Schema is exported from it

`schema.cue` already does shape, enums, and patterns in 251 lines against 1,172
lines of Go, and `cue.go` rewords its errors so the validation language never
reaches a user. v2 leans harder on what CUE is good at, because shorthand is a
disjunction problem: a service is a scalar or an object, a workload source is one
of three keys, a health check is a path or an exec form.

Alternatives considered. Hand-written Go validation gives better error text at
the cost of reimplementing closed-struct checking and every scalar constraint;
rejected because the error-rewording layer already closes most of that gap.
Generating the schema from Go structs inverts the dependency and loses CUE's
disjunctions; rejected for the same reason.

The JSON Schema is exported from the same CUE source at build time, so a release
cannot publish a schema that disagrees with the contract it enforces. Editor
completion is the user-visible payoff, and the export is verified by a test that
round-trips the corpus of valid and invalid fixtures through both.

### Normalization is a pipeline with one canonical output

```d2
direction: right

input: "ob.yml (v2)" {
  style.fill: "#eef6ff"
}
v1: "ob.yml (v1)" {
  style.fill: "#eeeeee"
}

cue: "CUE\nshape, enums, patterns" {
  style.fill: "#f5f5f5"
}
expand: "expand shorthand\nscalar -> object\ntop-level -> workload" {
  style.fill: "#f5f5f5"
}
defaults: "resolve defaults\nrecord origin per field" {
  style.fill: "#f5f5f5"
}
semantic: "Go\ncross-field + compose-semantic" {
  style.fill: "#f5f5f5"
}

model: "normalized model\n(one shape, both versions)" {
  style.fill: "#fff8e1"
}

gen: "generate runtime" {
  style.fill: "#e8f5e9"
}
plan: "seal plan\nbind runtime digest" {
  style.fill: "#e8f5e9"
}

input -> cue -> expand -> defaults -> semantic -> model
v1 -> semantic: "v1 loader, deprecated"
model -> gen -> plan
```

Expansion happens before defaults so that a defaulted value can never be
mistaken for a shorthand expansion, and both happen before cross-field checks so
those checks see one shape. The canonical form is printable, and printing it is
the contract for "what did Onebox understand", replacing the v1 habit of reading
the rendered Compose to find out.

**Default precedence**, highest first: an explicit value in the project file; a
value expanded from shorthand in the project file; a value derived from another
declared field (routing derived from a declared domain and port); a documented
contract default. Every field carries its origin through the normalized model,
and origins are reported in configuration output and in plans. There is no
host-derived tuning in this change — that arrives with drivers — so the chain has
no environmental input and normalization stays a pure function.

### The Compose merge boundary is enumerated, and conflicts are errors

A workload sourced by `compose: file#service` is copied verbatim, and Onebox
overlays exactly four things: attachment to the ingress network, release
identity labels, proxy routing labels derived from a declared domain, and the
rolling-deployment container naming that forbids a fixed container name.

The overlay set is closed and stated in the specification, not left to
implementation judgment. If the referenced service already sets one of those
keys, generation fails and names the conflict rather than overwriting it — the
same fail-closed posture the engine already takes on drift. Silently winning
would make the rendered runtime a poor explanation of what the user wrote, which
is precisely the failure mode that makes generated configuration untrustworthy.

Alternative considered: a general deep-merge with precedence rules. Rejected. A
deep merge is unpredictable at exactly the moment a user is debugging
production, and its rules are impossible to state in one sentence.

### Naming is derived, stable, and collision-checked before use

Generated projects, networks, and volumes derive their names from the
application identifier and, where applicable, the workload or service name.
Names are stable across releases so that a rollback cannot orphan a resource,
length-validated against Docker's limits, and truncated with a
collision-resistant suffix when a derived name would be too long.

Before generation completes, Onebox checks the target for a name collision with
a resource it does not own — determined by its own labels — and fails rather than
adopting or overwriting. Adoption of pre-existing resources is a separate
contract with its own evidence requirements and is not in scope.

### The generated runtime is bound into the plan by digest

The existing executable plan binds config, Compose, host state, images, rendered
Compose, and payload. v2 adds the generated runtime's digest to that binding.
At execution the runtime is regenerated from the plan's own inputs and the digest
compared; a mismatch is refused before any mutation.

This is what makes generation safe to trust: an operator or agent never has to
believe that the file executed matches the file rendered, because execution
proves it. It also means generation must be a pure function of the plan's
recorded inputs — hence the determinism requirement, and hence no wall-clock,
no map-order, and no undeclared environment input anywhere in the generator.

### Ejection is a one-way door, recorded in the project

Ejection writes the generated runtime to a path in the repository and sets the
affected workloads' source to a Compose reference into that file. From then on
they follow the Compose-referenced path above, including the enumerated overlay
and its conflict rules. Onebox does not track what it previously generated for
them and will not offer to re-adopt.

The alternative — a reversible eject that keeps generating alongside the ejected
file — was rejected because it produces two sources of truth for the same
service and no clear answer to which wins after a Onebox upgrade changes the
generator.

### v1 and v2 coexist behind one internal model

The v1 loader is left in place and maps into the same normalized model, so the
engine, planner, and approval paths see one shape and are untouched by this
change. Selection is by the declared schema identity, and a project with no
recognizable identity is rejected rather than guessed at.

This is what keeps the change survivable: if v2 generation has a defect, every
existing project can still be deployed by the path that has been working, and
the deprecation window is long enough to find out.

## Risks / Trade-offs

**A generator is a compatibility surface forever.** Once users depend on the
runtime Onebox produces, changing it changes their production. → The runtime
digest is bound into plans, so a generator change surfaces as a visible diff in
the next plan rather than as a silent change at execution. Generator changes that
alter output for an unchanged project are treated as breaking and require their
own change.

**The merge boundary is the hardest thing here.** A referenced Compose service
that sets one of the four overlaid keys is a real case — monk sets
`traefik.docker.network` deliberately, and for a documented reason. → The
conflict is an error naming the key and the file, and the resolution is to remove
it from the Compose file and declare the domain instead. This is a migration
cost, and the conversion capability reports it rather than discovering it at
deploy time.

**Two loaders means two paths to test.** → The v1 loader is frozen: no new
behavior lands in it during the window, and its existing tests are the
regression suite. The conversion capability's runtime-comparison requirement is
what proves the two paths agree.

**Shorthand expansion can surprise.** A user who writes top-level workload
fields and later adds a workload block may expect a merge. → Declaring both is a
validation error naming both locations, rather than a precedence rule nobody
remembers.

**Build-sourced workloads are incomplete until the release-pipeline change.**
`build:` normalizes but cannot resolve an image on its own yet. → Generation
fails closed with an error naming the interim mechanism, and the release-pipeline
change removes the gap. This is stated in the specification so it cannot be read
as a defect.

## Migration Plan

1. Ship v2 loading, generation, rendering, and ejection alongside an unchanged
   v1 path. No existing project changes behavior.
2. Ship conversion with its runtime comparison. Convert the four adopting
   projects, compare generated runtimes against their v1 runtimes, and resolve
   every reported difference before any of them deploys from a v2 project.
3. Express the project that declined v1 as a v2 project. If it cannot be
   expressed, the contract is wrong and this step blocks the rest.
4. Turn on the deprecation notice for v1 loading.
5. Remove v1 loading one release cycle later, as its own change.

Rollback: v2 is additive to the loader until step 5. Reverting means selecting
the v1 path, which is untouched. After step 5, rollback requires reinstating the
v1 loader, which is why it is a separate change with its own decision point.

## Open Questions

- Whether the ejection destination should default to a conventional path or
  always require an explicit one. Both are compatible with the specification and
  the choice can be made when the command is written.
- Whether the exported JSON Schema is published to a stable URL per release or
  only shipped in the binary for local reference. This affects distribution, not
  the contract.
