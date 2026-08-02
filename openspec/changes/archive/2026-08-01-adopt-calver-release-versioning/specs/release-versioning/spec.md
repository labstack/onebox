## Purpose

Defines sortable Onebox release identities and fail-closed runner compatibility
so automation can distinguish an approved release from a development build.

## ADDED Requirements

### Requirement: Releases use monthly CalVer identities
Every Onebox release SHALL have the form `vYEAR.MONTH.SEQUENCE`, where `YEAR`
has four digits, `MONTH` is zero-padded from `01` through `12`, and `SEQUENCE`
is an unpadded positive integer that restarts at one for the first release in a
calendar month. Release ordering SHALL compare year, month, and sequence as
integers.

#### Scenario: First release in a month
- **WHEN** no release tag exists for the current year and month
- **THEN** the next release identity ends in `.1`

#### Scenario: Additional release in a month
- **WHEN** the greatest valid release tag for the current year and month ends in `.7`
- **THEN** the next release identity ends in `.8`

#### Scenario: Calendar month changes
- **WHEN** the most recent release is from an earlier month and no valid tag exists for the current month
- **THEN** the next release uses the current zero-padded month and sequence `1`

### Requirement: Build provenance distinguishes releases from development
A build from an exact release tag SHALL report that tag as its version. A build
that is not at an exact release tag SHALL report Git-derived provenance that is
not accepted as an Onebox release identity. Dirty checkout provenance SHALL be
visibly marked dirty.

#### Scenario: Exact tagged build
- **WHEN** the binary is built from a commit checked out at an exact valid release tag
- **THEN** version output reports that release tag

#### Scenario: Commit after a tag
- **WHEN** the binary is built from a commit that is not exactly at a release tag
- **THEN** version output includes Git-derived development provenance and does not report a new release identity

#### Scenario: Dirty checkout
- **WHEN** tracked source differs from the checked-out commit at build time
- **THEN** version output marks the build dirty

### Requirement: Minimum runner policy uses CalVer ordering
When `minimum_onebox_version` is configured, the system SHALL require both the
configured minimum and the running version to be valid Onebox release
identities and SHALL compare their numeric year, month, and sequence. It SHALL
reject an older, development, malformed, or incomparable runner before remote
mutation.

#### Scenario: Runner meets the minimum
- **WHEN** the runner is `v2026.08.4` and the configured minimum is `v2026.08.3`
- **THEN** compatibility validation succeeds

#### Scenario: Runner is from an earlier month
- **WHEN** the runner is `v2026.07.20` and the configured minimum is `v2026.08.1`
- **THEN** compatibility validation fails before mutation with upgrade guidance

#### Scenario: Development runner is selected
- **WHEN** a minimum is configured and the runner version is commit-derived or otherwise not a valid release identity
- **THEN** compatibility validation fails before mutation and directs the caller to select a released binary

#### Scenario: Minimum is malformed
- **WHEN** `minimum_onebox_version` does not use the canonical CalVer form
- **THEN** configuration compatibility fails with an error identifying the invalid minimum

### Requirement: Release tag creation is guarded and retry-safe
The release workflow SHALL create the next CalVer tag only from a clean tracked
`main` branch whose commit matches `origin/main`, after repository checks pass.
It SHALL fetch remote tags before calculating the sequence and SHALL leave no
new local tag when publication fails.

#### Scenario: Branch is not releasable
- **WHEN** the current branch is not `main`, tracked changes exist, checks fail, or local `main` differs from `origin/main`
- **THEN** the workflow exits without creating or publishing a release tag

#### Scenario: Tag publication races
- **WHEN** another publisher claims the calculated tag before this workflow publishes it
- **THEN** publication fails and the workflow removes its unpublishable local tag so a retry can refetch and recalculate
