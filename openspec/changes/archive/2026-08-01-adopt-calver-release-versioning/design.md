## Context

See `proposal.md` for motivation and `specs/release-versioning/spec.md` for the
observable contract. Onebox links a release string into binaries and exposes a
minimum-runner policy. Both need to use the same strict CalVer grammar and
numeric ordering, including a zero-padded month.

The repository has one releasable binary and therefore needs one unprefixed
repository tag namespace. Release publication remains an explicit maintainer
operation; LLM-facing MCP tools do not create tags or releases.

## Goals / Non-Goals

**Goals:**

- Make the binary's version sufficient to distinguish an exact release from a
  checkout build.
- Compare environment minimums using the exact release grammar and numeric
  ordering.
- Make repeated monthly releases deterministic and safe to retry.

**Non-Goals:**

- Provide an MCP release tool, automatic changelog generation, package
  signing, artifact publication, or a hosted CI release pipeline.
- Interpret arbitrary version strings, dates, prerelease suffixes, or
  Git-describe strings as approved releases.
- Change executable-plan schemas or remote target state.

## Decisions

### 1. Use `vYYYY.MM.N` and a dedicated parser

The canonical grammar is `^v[0-9]{4}\.(0[1-9]|1[0-2])\.[1-9][0-9]*$`.
Comparison parses the three fields as integers and orders year, month, then
sequence. User-authored minimums must include the `v` prefix so configuration,
tags, version output, and plan provenance all use one representation.

A dedicated parser keeps the grammar, validation errors, and numeric ordering
explicit. Reformatting months without padding was rejected because it would
diverge from the established repository release convention. Quietly accepting
both forms was rejected because aliases make policy and evidence less
deterministic.

### 2. Use Git description for non-release build identity

The build recipe obtains its default version from `git describe --tags
--always --dirty`. An exact tag produces the release identity. Later commits,
untagged repositories, and dirty checkouts produce descriptive provenance that
the release parser refuses. Linker injection remains available through
`OB_VERSION` for reproducible packaging and accepts only canonical CalVer.

Using the next anticipated CalVer for development was rejected because it can
make unreviewed code appear released. A generic constant alone was rejected
because it loses useful commit provenance.

### 3. Treat a configured minimum as a released-runner gate

If no minimum is configured, development builds remain usable for local work.
If a minimum exists, both values must parse as canonical CalVer; malformed
policy is reported separately from an unreleased runner. This preserves local
iteration while making production-oriented environment policy fail closed.

Only canonical Onebox CalVer is accepted. Carrying multiple incomparable
version domains would make ordering ambiguous. Existing project examples are
updated in the same change.

### 4. Calculate and publish tags under guarded local release automation

The release recipe fetches `origin/main` and all tags, requires clean tracked
state on `main`, requires `HEAD == origin/main`, runs the normal repository
checks, revalidates remote state, filters the UTC month's tags to canonical
numeric sequences, and selects the greatest sequence plus one. It creates a
metadata-only release commit whose parent and tree are the checked commit, then
atomically fast-forwards `main` to that commit and creates the tag at it under
an exact lease on the checked remote `main`. A failed push deletes only the
local tag the recipe just created and leaves local `main` unchanged. A
concurrent branch or tag publisher therefore causes a safe failure; the next
run refetches and chooses from current state.

A no-op `main` refspec is insufficient because Git may omit an already-up-to-
date ref from the remote transaction. The real metadata-only fast-forward makes
the branch compare-and-swap and tag creation one server-side atomic operation.
The workflow does not rewrite history, force-update an unexpected branch, or
delete or replace remote tags. Artifact publication can later subscribe to the
tag namespace without changing this contract.

## Risks / Trade-offs

- [Git-describe can select a non-release tag] -> Minimum-runner validation
  accepts only canonical release syntax, and releases use a dedicated valid-tag
  filter when calculating the sequence.
- [Two maintainers can calculate the same next sequence] -> The exact branch
  lease and tag creation share one atomic transaction; the loser removes its
  local tag and retries without advancing `main`.
- [A manually supplied `OB_VERSION` can be misleading in display-only use] ->
  Production-oriented minimum policy validates it, and VCS revision plus dirty
  state remain independently visible.
- [Changing version domains rejects prior minimum values] -> Update examples
  now, before production rollout, and return a specific invalid-minimum error.

## Migration Plan

1. Add the CalVer parser, ordering tests, Git-derived build default, and guarded
   release recipe.
2. Replace release-shaped development examples with canonical CalVer examples
   and document the released-runner gate.
3. Run unit, static, OpenSpec, and release-recipe dry validation before marking
   the change complete.
4. Create the first tag only in a separate authorized release operation from a
   clean, up-to-date `main` branch.

Rollback before the first tag restores the previous comparison and build
default. After CalVer-tagged binaries or policies exist, rollback requires
removing `minimum_onebox_version` temporarily or restoring a compatible
CalVer-aware runner; no remote application state needs migration.
