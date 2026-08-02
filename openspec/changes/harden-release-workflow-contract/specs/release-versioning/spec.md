## MODIFIED Requirements

### Requirement: Releases use monthly CalVer identities

Every Onebox release SHALL have the form `vYEAR.MONTH.SEQUENCE`, where `YEAR`
has four digits, `MONTH` is the zero-padded UTC calendar month from `01`
through `12`, and `SEQUENCE` is an unpadded positive integer that restarts at
one for the first release in a UTC calendar month. Release ordering SHALL
compare year, month, and sequence as integers.

#### Scenario: First release in a UTC month
- **WHEN** no release tag exists for the current UTC year and month
- **THEN** the next release identity ends in `.1`

#### Scenario: Additional release crosses a decimal boundary
- **WHEN** the greatest valid release tag for the current UTC year and month ends in `.9`
- **THEN** the next release identity ends in `.10`

#### Scenario: UTC calendar month changes
- **WHEN** the most recent release is from an earlier UTC month and no valid tag exists for the current UTC month
- **THEN** the next release uses the current zero-padded UTC month and sequence `1`

### Requirement: Minimum runner policy uses CalVer ordering

When `minimum_onebox_version` is configured, the system SHALL require both the
configured minimum and the running version to be valid Onebox release
identities and SHALL compare their numeric year, month, and sequence. It SHALL
reject an older, development, malformed, or incomparable runner before
lifecycle planning or lifecycle execution. The explicit break-glass `ob exec`
adapter is not a lifecycle planning or execution operation and SHALL remain
outside this minimum-runner gate.

#### Scenario: Runner meets the minimum
- **WHEN** the runner is `v2026.08.4` and the configured minimum is `v2026.08.3`
- **THEN** lifecycle compatibility validation succeeds

#### Scenario: Runner is from an earlier month
- **WHEN** the runner is `v2026.07.20` and the configured minimum is `v2026.08.1`
- **THEN** lifecycle compatibility validation fails before planning or execution with upgrade guidance

#### Scenario: Development runner is selected
- **WHEN** a minimum is configured and the lifecycle runner version is commit-derived or otherwise not a valid release identity
- **THEN** lifecycle compatibility validation fails before planning or execution and directs the caller to select a released binary

#### Scenario: Minimum is malformed
- **WHEN** `minimum_onebox_version` does not use the canonical CalVer form
- **THEN** lifecycle configuration compatibility fails with an error identifying the invalid minimum

#### Scenario: Operator invokes the break-glass exec adapter
- **WHEN** an operator explicitly invokes `ob exec` for a project with `minimum_onebox_version` configured
- **THEN** that separate break-glass operation proceeds without lifecycle minimum-runner validation and remains subject to its own authorization and safety controls

### Requirement: Release tag creation is guarded and retry-safe

The release workflow SHALL create the next CalVer tag only from a clean tracked
`main` branch whose commit matches `origin/main`, after repository checks pass.
It SHALL fetch remote tags before calculating the sequence and create a
metadata-only release commit whose sole parent is the checked commit and whose
tree is identical to the checked tree. Publication SHALL send both that release
commit to `refs/heads/main` and a new tag pointing to the release commit in one
atomic push, with an exact lease requiring remote `main` still to equal the
checked commit. The branch update SHALL be a fast-forward and SHALL NOT rewrite
existing history. When the lease or atomic publication fails, the workflow
SHALL remove its newly created local tag, SHALL NOT publish its release commit
or tag, and SHALL leave local `main` at the checked commit. After successful
remote publication, it SHALL advance local `main` to the release commit when
the local ref still equals the checked commit; otherwise it SHALL report that
publication succeeded and require the maintainer to synchronize the checkout.

#### Scenario: Branch is not releasable
- **WHEN** the current branch is not `main`, tracked changes exist, checks fail, or local `main` differs from `origin/main`
- **THEN** the workflow exits without creating or publishing a release tag

#### Scenario: Main advances while checks run
- **WHEN** `origin/main` advances after initial validation and before the workflow revalidates after repository checks
- **THEN** the workflow exits without creating or publishing a release tag

#### Scenario: Main advances immediately before publication
- **WHEN** `origin/main` advances after final fetch and before the atomic publication is accepted
- **THEN** the exact branch lease rejects the atomic push, remote `main` retains the newer commit, the attempted remote tag does not exist, and local `main` and the local tag remain at their pre-publication state

#### Scenario: Tag publication races
- **WHEN** another publisher claims the calculated tag before this workflow publishes it
- **THEN** atomic publication does not advance `main` or replace the winning remote tag, and the workflow removes its unpublishable local tag so a retry can refetch and recalculate

#### Scenario: Guarded publication succeeds
- **WHEN** local and remote `main` remain at the checked commit and the calculated tag is unclaimed
- **THEN** the atomic publication fast-forwards remote `main` to the metadata-only release commit and creates the remote tag at that same commit, after which local `main` advances to the published commit

#### Scenario: Local branch changes after remote publication starts
- **WHEN** remote publication succeeds but local `main` no longer equals the checked commit before the local ref update
- **THEN** the workflow preserves the published release, does not overwrite the changed local branch, and reports that the maintainer must synchronize the checkout
