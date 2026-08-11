## MODIFIED Requirements

### Requirement: Releases use monthly CalVer identities

Every Onebox release SHALL have the form `vYYYY.M.REVISION`, where `YYYY` is
the four-digit UTC Gregorian year, `M` is the unpadded UTC calendar month from
`1` through `12`, and `REVISION` is an unpadded non-negative integer of at most
nineteen digits that restarts at zero for the first release in a UTC calendar
month. Release ordering SHALL compare year, month, and revision as integers.
The bound is the width every implementation can hold and compare: a wider
revision would be publishable as a tag and unusable as runner provenance.

#### Scenario: First release in a UTC month
- **WHEN** no release tag exists for the current UTC year and month
- **THEN** the next release identity is `vYYYY.M.0` for that month

#### Scenario: Additional release crosses a decimal boundary
- **WHEN** the greatest valid release tag for the current UTC year and month ends in `.9`
- **THEN** the next release identity ends in `.10`

#### Scenario: UTC calendar month changes
- **WHEN** the most recent release is from an earlier UTC month and no valid tag exists for the current UTC month
- **THEN** the next release uses the current four-digit UTC year, unpadded UTC month, and revision `0`

#### Scenario: Zero-padded month is presented
- **WHEN** a release identity contains a zero-padded month such as `v2026.08.0`
- **THEN** release validation rejects it as non-canonical

#### Scenario: Abbreviated year is presented
- **WHEN** a release identity abbreviates the year such as `v26.8.0`
- **THEN** release validation rejects it as non-canonical

#### Scenario: Revision is zero-padded
- **WHEN** a release identity contains a zero-padded revision such as `v2026.8.00`
- **THEN** release validation rejects it as non-canonical

#### Scenario: Revision exceeds the representable width
- **WHEN** a release identity carries a revision wider than nineteen digits, or the next revision for a month would exceed it
- **THEN** release validation and release creation both reject it rather than publishing an identity the runner cannot parse

### Requirement: Minimum runner policy uses CalVer ordering

When `minimum_onebox_version` is configured, the system SHALL require both the
configured minimum and the running version to be valid Onebox release
identities and SHALL compare their numeric year, month, and revision. It
SHALL reject an older, development, malformed, or incomparable runner before
lifecycle planning or lifecycle execution. The explicit break-glass `ob exec`
adapter is not a lifecycle planning or execution operation and SHALL remain
outside this minimum-runner gate.

#### Scenario: Runner meets the minimum
- **WHEN** the runner is `v2026.8.4` and the configured minimum is `v2026.8.3`
- **THEN** lifecycle compatibility validation succeeds

#### Scenario: Runner is from an earlier month
- **WHEN** the runner is `v2026.7.20` and the configured minimum is `v2026.8.0`
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
