## Why

The `onebox.run/v1` contract is validated by CUE. Building the contract against
it exposed what that choice actually costs here, and the costs are not the ones
anyone expects from a schema language.

**It was not closed.** `#Source` was a disjunction of open branches — open
because each branch has to unify with the rest of a workload — and that openness
propagated through every definition embedding it. `workloads: {web: {image:
nginx, replicaz: 3}}` validated, deployed, and ran one replica. The most common
authoring mistake there is went unreported by a contract whose entire argument
is that it catches things before a host does. Closedness in CUE is a property
you have to keep getting right, and getting it wrong is silent.

**Its failures name the wrong thing.** A workload is a disjunction over four
roles, so a value failing every branch is reported against whichever branch the
validator tried first. A typo in a worker came back as `role: conflicting values
"application" and "worker"`, sending the author to correct the one field that
was right. Fixing that took a rewording layer and a per-role pre-validation pass
in Go — 183 lines whose only job is to make CUE's output usable.

**It cannot publish the contract.** `encoding/openapi` cannot represent `!=`, so
there is no mechanical path from the schema to a JSON Schema an editor can read.
Today the contract is formally specified in a language nothing but Onebox can
consume, and an author writing `ob.yml` gets no completion, no hover
documentation, and no inline error.

**Its distinctive feature is unused.** CUE is for unifying configuration from
several sources. Onebox validates one document. Environment overrides — the one
place unification would apply — are implemented in Go with a closed allowlist,
deliberately, because layered merge semantics nobody can predict while reading
is the failure mode this contract exists to avoid.

Meanwhile the parser already in use does the hard part. `yaml.v3`'s
`Decoder.KnownFields(true)` rejects an unknown field by name and line number,
which is both stricter and clearer than what CUE produced. And shorthand
expansion — the union handling that plain structs are worst at — already runs in
Go across eighteen keys before CUE sees the document at all.

## What Changes

- Replace CUE validation with typed Go structs decoded under
  `KnownFields(true)`, so closedness is a property of the type rather than a
  semantic to re-derive.
- Move enums, patterns, and bounds into one declarative validation table, so a
  constraint and the sentence explaining its violation live together and can be
  read as the contract.
- Apply defaults in an explicit pass. CUE's defaulting produced a real defect in
  this contract — defaults on optional fields never materialise — and explicit
  assignment cannot fail that way.
- Select a workload's shape by its declared role rather than by disjunction
  discrimination, which is what the current pre-validation pass already does.
- Publish a JSON Schema generated from the same structs, referenced from
  scaffolded projects, so editors can read the contract.
- Delete `internal/app/schema.cue`, the rewording layer, the per-role
  pre-validation pass, and the `cuelang.org/go` dependency.

Cross-field rules do not move. The twenty-three typed errors the loader already
raises — source exclusivity, prerequisite resolution, route collisions, proxy
defaulting, derived-name limits, identifier uniqueness, driver validation — are
already Go and stay exactly where they are.

## Impact

No change to the authoring contract. `api_version` stays `onebox.run/v1`, every
field keeps its shape, and every project that loads today loads afterwards
unchanged. This is an implementation replacement, and the evidence that it is
one is that the corpus does not move: sixty-five conformance cases accept and
reject identically, nineteen real projects load, and their generated runtimes
stay byte-identical.

Authors gain editor support and better failures. Operators see no behavioural
difference. The contract gains a published artifact that something other than
Onebox can read.

`openspec/config.yaml` states that "CUE provides its closed, versioned schema".
That sentence becomes false when this lands and is updated with it — a context
that contradicts the code makes every artifact generated from it wrong.

## Non-Goals

- No change to what the contract accepts or rejects. A change in the corpus is
  a defect in this work, not a decision.
- No change to generated runtimes. Byte-identical output is the acceptance test.
- No change to the cross-field rules, their error codes, or their messages.
- No JSON Schema as the source of truth. It is an output, generated and
  corpus-gated; making it the source would weaken defaults and cross-field
  reasoning while keeping every Go rule that exists today.
- No new validation capability, no new fields, and no relaxation of an existing
  constraint under cover of the migration.
- This change does not begin until `adopt-declarative-project-schema` archives.
  Replacing the validator of a contract that is still being defined would make
  both unreviewable.
