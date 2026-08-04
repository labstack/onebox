# Design

## Context

The contract is enforced today by 335 non-comment lines of CUE plus 183 lines of
Go whose only purpose is to make CUE's output usable — a rewording layer, a
path extractor, a near-miss suggester, and a per-role pre-validation pass added
because the disjunction reports the wrong branch. Alongside that sit 23 typed
cross-field errors that were always Go, because CUE either could not express
them or expressed them in a way that broke something else.

The measurement that decides this: of the 335 CUE lines, 116 are field
declarations, 34 are defaults, 30 are `_|_` negations existing only to make role
discrimination work, 28 are enums, and 20 are patterns. Nothing in that list
requires unification.

## Goals

- Closedness that cannot be silently lost.
- Failures that name the field, the line, and the constraint.
- A published schema an editor can read.
- Byte-identical behaviour, proven against the existing corpus.

## Non-Goals

- Changing the contract. This is an implementation replacement.
- Moving cross-field rules. They are already where they belong.

## Decisions

### Typed structs decoded under `KnownFields(true)`

`yaml.v3` rejects an unknown field with its name and line number:

    line 3: field replicaz not found in type app.Workload

Closedness becomes a property of the type. There is no composition rule to get
right, which is the failure mode being removed: `#Source`'s open branches made
every workload accept every field, and nothing said so.

**Alternative considered — keeping CUE and closing it correctly.** Possible; it
is what the current code does after four separate discoveries. It leaves the
error-message layer, the export blocker, and the next subtlety in place.

### Shorthand stays in `expand()`

Union handling is what plain structs are worst at, and this contract is full of
unions — `services: {postgres: 17}`, `image: nginx`, command as string or list,
`needs` as name or object. It is already solved: `expand()` normalises eighteen
keys on the raw map before validation, and it stays exactly as it is. Structs
see a normalised document.

This is also why the CUE disjunctions are cheaper to remove than they look: most
of them re-describe a shape `expand()` has already produced.

### One validation table for enums, patterns, and bounds

A constraint and the sentence explaining its violation live on the same line, in
one file, greppable by field path. Scattering `regexp.MustCompile` through
imperative code is how constraints drift from the documentation that describes
them — which is the thing CUE was chosen to prevent, and the part of it worth
keeping.

### Role selects the shape

A `switch` on `role` chooses which struct a workload decodes into. This is what
`validateWorkloadShapes` already does at runtime; making it the only mechanism
removes the disjunction, the 30 `_|_` negations, and the misdirected errors
together.

### JSON Schema is generated, not authoritative

Generated from the struct declarations and the validation table, embedded, and
gated against the conformance corpus so it cannot drift from what is enforced.

**Alternative considered — JSON Schema as the source.** Rejected: weaker at
defaults and cross-field reasoning, and every Go rule that exists today would
still exist. It is the right output and the wrong input.

## Risks

**The contract moves without anyone noticing.** This is the only serious risk,
and it is why the acceptance test is the corpus rather than a review: 65
conformance cases, 19 real projects, and byte-identical generated runtimes. A
constraint that is awkward to reproduce gets reproduced.

**A pattern is transcribed wrongly.** Each is moved with its conformance case,
and the hostile-value tests added during hardening cover the grammars that reach
generated files.

**Error text regresses for a case nobody tested.** The typed error codes are
asserted by the corpus; message wording is checked for the cases the corpus
names and improved where it is currently poor.

## Migration

Sequenced after `adopt-declarative-project-schema` archives. Replacing the
validator of a contract still being defined would make both unreviewable, and
the corpus that proves equivalence is only trustworthy once it has stopped
moving.

Within the change, the order is: structs and decoding, then the validation
table, then defaults, then deletion of CUE, then the published schema. The
corpus runs at every step, and CUE is not removed until the replacement passes
it.
