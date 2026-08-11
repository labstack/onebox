## MODIFIED Requirements

### Requirement: Releases use monthly CalVer identities

Every Onebox release SHALL have the form `vYY.M.SEQUENCE`, where `YY` is the
two-digit UTC Gregorian year from `10` through `99` (mapping to 2010 through
2099), `M` is the unpadded UTC calendar month from `1` through `12`, and
`SEQUENCE` is an unpadded positive integer that restarts at one for the first
release in a UTC calendar month. Release ordering SHALL compare the mapped
year, month, and sequence as integers. The release publisher SHALL refuse a
calendar year outside the supported range rather than emit a tag that is not
both canonical Onebox CalVer and valid semantic-version syntax.

#### Scenario: First release in a UTC month
- **WHEN** no release tag exists for the current UTC year and month
- **THEN** the next release identity is `vYY.M.1` for that month

#### Scenario: Additional release crosses a decimal boundary
- **WHEN** the greatest valid release tag for the current UTC year and month ends in `.9`
- **THEN** the next release identity ends in `.10`

#### Scenario: UTC calendar month changes
- **WHEN** the most recent release is from an earlier UTC month and no valid tag exists for the current UTC month
- **THEN** the next release uses the current unpadded UTC month and sequence `1`

#### Scenario: Zero-padded month is presented
- **WHEN** a release identity contains a zero-padded month such as `v26.08.1`
- **THEN** release validation rejects it as non-canonical

#### Scenario: Release year is outside the supported epoch
- **WHEN** the UTC year cannot be represented by canonical `YY` in the 2010 through 2099 epoch
- **THEN** release publication fails without creating or publishing a tag

### Requirement: Minimum runner policy uses CalVer ordering

When `minimum_onebox_version` is configured, the system SHALL require both the
configured minimum and the running version to be valid Onebox release
identities and SHALL compare their mapped numeric year, month, and sequence. It
SHALL reject an older, development, malformed, or incomparable runner before
lifecycle planning or lifecycle execution. The explicit break-glass `ob exec`
adapter is not a lifecycle planning or execution operation and SHALL remain
outside this minimum-runner gate.

#### Scenario: Runner meets the minimum
- **WHEN** the runner is `v26.8.4` and the configured minimum is `v26.8.3`
- **THEN** lifecycle compatibility validation succeeds

#### Scenario: Runner is from an earlier month
- **WHEN** the runner is `v26.7.20` and the configured minimum is `v26.8.1`
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
