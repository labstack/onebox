## Why

Onebox needs one unambiguous calendar-version release identity that matches its
release cadence, remains sortable, and lets agents fail closed when a checkout
or incompatible runner is selected.

## What Changes

- Adopt release tags and reported release versions in the form
  `vYEAR.MONTH.SEQUENCE`, with a zero-padded month and a positive sequence that
  restarts at one each calendar month.
- Derive non-release build versions from Git provenance instead of assigning a
  release-shaped placeholder.
- Add a guarded release recipe that calculates the next monthly sequence and
  only tags an up-to-date, clean `main` branch after checks pass.
- **BREAKING**: interpret `minimum_onebox_version` as Onebox CalVer. Invalid,
  development, commit-derived, or incomparable versions fail closed whenever
  a minimum is configured.
- Document the release and minimum-runner contract in current public guides.
- This change does not alter target state, managed-service ownership, data
  formats, provider behavior, upgrade plans, or any destructive operation.

## Capabilities

### New Capabilities

- `release-versioning`: CalVer syntax, ordering, build provenance, guarded tag
  creation, and minimum-runner compatibility behavior.

### Modified Capabilities

None. The repository has no archived capability specifications yet.

## Impact

- Build and release automation in `Justfile`.
- Build provenance in `internal/buildinfo`.
- Runner compatibility enforcement in `internal/onebox/runner_policy.go`.
- Version-policy tests and the public build/schema documentation.
- No runtime dependency or remote-host layout change.
