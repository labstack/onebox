## Why

Post-merge review found that the shipped release contract under-specifies its
UTC clock, branch compare-and-swap publication guard, and executable race
evidence. Tightening the contract now keeps a future private release
deterministic and fail-closed without publishing a tag or changing runtime
application behavior.

## What Changes

- Define the release month using UTC and demonstrate integer sequence ordering
  across the `.9` to `.10` boundary.
- Require release publication to create a metadata-only fast-forward release
  commit and atomically compare-and-swap `origin/main` from the checked commit
  to that release commit while creating its tag, so an intervening branch
  advance cannot publish a stale release.
- Scope the configured minimum-runner gate to lifecycle planning and execution;
  the break-glass `ob exec` adapter remains outside that lifecycle contract.
- Extract the guarded release workflow into a testable script and add
  disposable local-repository integration coverage for normal sequencing and
  both check-time and publication-time races.
- Correct the archived design and review record so its claims match the
  hardened contract and executable evidence.

No release, tag, artifact, provider operation, remote application mutation, or
data migration is performed by this change. It does not add automatic releases,
destructive operations, service upgrades, or provider-specific behavior.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `release-versioning`: Clarify the UTC CalVer clock, lifecycle gate boundary,
  and atomic branch-lease plus tag-publication contract.

## Impact

- **User-visible behavior:** A future `just release` uses the UTC month,
  fast-forwards `main` by one metadata-only commit whose tree is the checked
  tree, and refuses publication if `origin/main` changes first.
- **Compatibility:** The CalVer grammar is unchanged. The minimum-runner policy
  is clarified to cover lifecycle planning and execution, not the explicit
  break-glass exec adapter.
- **Ownership:** Maintainers still initiate releases explicitly; MCP tools and
  coding agents do not gain release authority.
- **Implementation and tests:** The `Justfile` delegates to a repository script,
  and Go integration tests exercise it only against temporary local Git
  repositories.
- **Documentation:** The canonical release specification and archived design
  evidence become precise about UTC, compare-and-swap publication, and tested
  race behavior.
