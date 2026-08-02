## Context

See `proposal.md` for motivation and
`specs/release-versioning/spec.md` for the observable delta. The guarded
workflow must close state races both while checks run and immediately before
publication. Git omits an already-up-to-date branch from the actual ref
transaction, so the exact branch lease cannot close the final race unless the
transaction contains a real branch update. The release clock and executable
race evidence must also be deterministic across maintainer hosts.

Git defines `--atomic` as an all-or-nothing update of every named ref and
`--force-with-lease=<ref>:<expect>` as permission to update that ref only while
its remote value is exactly the expected object. The design relies on those
documented semantics: <https://git-scm.com/docs/git-push>.

## Goals / Non-Goals

**Goals:**

- Make UTC the only clock used to choose a monthly release namespace.
- Specify and test a real fast-forward branch compare-and-swap as part of
  tag-publication serialization.
- Exercise the actual workflow against disposable repositories, including
  state changes at both race boundaries.
- Keep the maintainer-facing recipe small enough to audit.

**Non-Goals:**

- Publish a real tag, release, artifact, or branch update.
- Add hosted CI, automatic release initiation, an MCP release tool, signing,
  or changelog generation.
- Gate the support-oriented `ob exec` adapter or move lifecycle authority out
  of the canonical Go service.
- Change service data, provider state, application configuration, or upgrade
  policy.

## Decisions

### 1. Select the release namespace with the UTC calendar

The workflow uses `date -u +%Y.%m`. Build timestamps already use UTC, so this
removes the only host-time-zone dependency from release identity selection.
Accepting the maintainer's local month was rejected because two eligible
release hosts can disagree near a month boundary.

### 2. Publish a metadata-only release commit and tag atomically

After checks and tag calculation, the workflow creates a commit with
`git commit-tree`. Its parent is the checked `main` commit, its tree is exactly
the checked tree, and its message records the release identity. This produces
a real fast-forward ref update without changing source files or running
post-check code. A lightweight tag created with `--no-sign` points to this
release commit so inherited local signing configuration cannot introduce a
prompt or silently change the tag representation; signing remains a separately
designed non-goal.

Publication names two refspecs in one `git push --atomic`: the release commit
to `refs/heads/main`, and the new local tag to its remote tag ref. An exact
`--force-with-lease=refs/heads/main:<checked-commit>` makes the branch update a
compare-and-swap. If `main` advances before the server accepts the push, its
lease fails and atomicity prevents tag creation. If the tag is claimed, that
ref update fails and atomicity prevents the release commit from advancing
`main`. The workflow deletes only its new local tag on failure; its local
branch was never moved. After success it advances local `main` to the published
commit with an exact local ref compare-and-swap; a failure to perform that
local convenience update is warned but cannot undo or misreport publication.

A no-op branch refspec was rejected because executable testing shows that Git
classifies it as already up to date and omits it from the remote ref
transaction. A tag-only push and a final fetch without a push lease were also
rejected because each leaves a race window after validation. Rewriting or
force-advancing existing branch history is not permitted; the release commit
is an ordinary one-parent fast-forward.

### 3. Put the workflow in a script with the recipe as a thin adapter

`scripts/release.sh` owns validation, checks, sequence calculation, tagging,
and publication. `just release` invokes that script and contains no duplicate
release logic. The script remains an explicit maintainer operation; it is not
called by MCP or product lifecycle code.

Keeping the multiline implementation embedded in the `Justfile` was rejected
because an integration test cannot invoke exactly the release behavior without
also depending on recipe parsing and task dispatch.

### 4. Test real Git behavior against disposable local remotes

A Go integration test creates a temporary bare `origin`, a `main` checkout,
and a second publisher checkout. It invokes the production script with a
temporary `just` shim so repository checks are deterministic while Git
validation and publication remain real.

The harness covers first-of-month selection, `.9` to `.10`, prior-month reset,
an `origin/main` advance during checks, an advance triggered immediately before
push, and a competing publisher claiming the tag. Successful cases prove that
the release commit has the checked commit as its parent, preserves its tree,
and is the object named by both remote `main` and the tag. Failure cases assert
that the attempted local tag is removed, no partial branch/tag transaction is
published, and competing remote state survives. The current UTC month is
injected only through the process clock; expected tags are calculated with UTC
so the test exercises the production clock choice.

Mocking `git` itself was rejected because it would only test shell branching,
not atomic ref updates or lease enforcement. The harness never names the real
repository remote and cannot publish a project tag.

### 5. Preserve the product boundary of the minimum-runner gate

The configured minimum remains a lifecycle compatibility policy applied before
planning or execution. `ob exec` is an explicit support and break-glass adapter,
not a lifecycle plan or convergence entry point, so the release specification
does not imply it is gated. Expanding the gate would be a separate behavioral
and security design requiring its own proposal.

## Risks / Trade-offs

- [A Git server does not support atomic pushes] -> Publication fails closed and
  the local tag is removed; maintainers must not fall back to a non-atomic push.
- [A check-time race occurs before a local tag exists] -> The second fetch and
  equality check stop without cleanup because nothing was created.
- [A publication-time race occurs after local tag creation] -> The real
  fast-forward update, exact lease, and atomic push prevent either attempted
  ref from publishing, and the failure path deletes the local tag.
- [The release commit adds history without source changes] -> Its parent and
  tree are mechanically constrained, it records the released identity, and it
  is the minimal portable way to make generic Git servers enforce the branch
  compare-and-swap in the tag transaction.
- [Integration tests depend on Git hook behavior] -> Use only documented local
  Git repositories and assertions on final refs; skip with a clear reason when
  Git is unavailable.
- [UTC changes the selected namespace near a local month boundary] -> This is
  the intended deterministic behavior and is specified before any tag exists.

## Migration Plan

1. Add and strictly validate the OpenSpec delta.
2. Extract the existing recipe into `scripts/release.sh`, switch its month
   calculation to UTC, and publish a metadata-only release commit under the
   exact atomic branch lease.
3. Add the disposable-repository integration harness and run it with the full
   Go, race, static, docs, and OpenSpec checks.
4. Correct the archived design and evidence record, then archive this change so
   its delta updates the canonical specification.

Rollback restores the inline recipe and the prior canonical wording. No tag,
branch, runtime, provider, or application-state migration is involved.
