## Context

See `proposal.md` for motivation. Today `just release` validates a clean,
up-to-date `main`, creates a metadata-only commit and CalVer tag, and publishes
both atomically. No workflow consumes that tag, and the repository has no
binary/package release configuration. The current root Go module is
`github.com/labstack/onebox`, the only user-facing executable is `ob`, and the
repository has no public license declaration, Homebrew tap, WinGet fork, Scoop
bucket, Apple release credentials, or cross-repository publishing credentials.
This change makes dedicated `labstack/homebrew-tap` and
`labstack/scoop-bucket` repositories, a token scoped to those repositories, and
a shared LabStack macOS signing/notarization identity preconditions for the
first release tag.

GoReleaser requires semantic-version-compatible tags
(https://goreleaser.com/resources/limitations/semver/). Go modules separately
require a `/vN` module-path suffix for semantic major versions above one
(https://go.dev/ref/mod#major-version-suffixes). The release design therefore
treats CalVer as an application distribution identity and does not claim that
the root module is installable at that tag through `go install`.

## Goals / Non-Goals

**Goals:**

- Preserve the existing guarded, atomic maintainer entry point while producing
  one conventional tag syntax accepted by GoReleaser and package metadata.
- Make the tag workflow unable to publish before the same repository and Docker
  gates required of normal changes pass.
- Produce a complete, predictable archive/package matrix from one pinned
  configuration and validate it before the first real tag.
- Sign and notarize both macOS binaries before archiving them and publish a
  generated Homebrew Cask for both supported macOS architectures.
- Publish a tested Scoop manifest for both supported Windows architectures.
- Keep generated release artifacts out of the source tree and keep all workflow
  credentials least-privileged.

**Non-Goals:**

- Supporting `go install` at CalVer tags or presenting Onebox as a versioned Go
  library.
- Publishing WinGet, APT, or RPM repository metadata.
- Attesting or generating SBOMs in the first slice.
- Changing target-host behavior, application data, backup/restore ownership,
  deployment upgrades, or any lifecycle safety gate.

## Decisions

### Use `vYYYY.M.REVISION` with zero-based monthly revisions

The parser and publisher use the full four-digit UTC year, an unpadded month,
and an unpadded non-negative monthly revision that starts at zero. This keeps
the release identity immediately legible while remaining valid semantic-version
syntax. Numeric parsing remains the ordering authority; lexical ordering is
never used.

Alternatives rejected:

- `vYYYY.0M.SEQUENCE` is the previous readable form but GoReleaser rejects tags
  containing numeric components such as `08`.
- `vYY.M.SEQUENCE` is shorter, but hides the century and makes release output
  less immediately recognizable than the requested full-year form.
- Ordinary `v0.x.y`/`v1.x.y` would preserve `go install`, but would discard the
  requested time-based release identity.

### Keep `just release` as the only tag creator

The existing release script remains the maintainer entry point and retains its
clean-tree checks, post-check refetch, metadata-only commit, atomic push, and
exact branch lease. It changes only tag calculation and validation. The remote
workflow independently validates the tag and verifies that its commit is
reachable from `origin/main`, so a manually pushed lookalike tag cannot bypass
the normal lineage boundary.

### Reuse CI as a callable verification gate

The existing CI workflow gains `workflow_call`. The release workflow calls it
and publishes only after every called job succeeds. This avoids a second copy
of repository and Docker verification. The normal CI path also validates a
GoReleaser snapshot, so configuration failures are found in pull requests
rather than after an immutable tag exists.

Native Linux, macOS, and Windows smoke jobs compile and execute safe local CLI
surfaces. GoReleaser snapshot validation separately constructs all six
GOOS/GOARCH archives and four Linux packages. Native smoke tests validate OS
behavior; cross-build validation covers the full artifact matrix.

### Pin one GoReleaser implementation and one artifact definition

`.goreleaser.yaml` is the only artifact definition. CI and release use
GoReleaser `v2.17.1` through the official v7 action pinned to commit
`f06c13b6b1a9625abc9e6e439d9c05a8f2190e94`, following the upstream action
requirements for full history and `release --clean`
(https://goreleaser.com/customization/ci/actions/).

The build injects the exact tag and build time through the existing private
`buildinfo` linker variables. Standard Go VCS metadata continues to provide
revision, source time, dirty state, and Go version. Archives use tar.gz except
for Windows ZIP files. nFPM produces Debian and RPM packages that install
`/usr/bin/ob`. Package metadata records the repository's current unlicensed
state as `Proprietary`; adopting a public license requires an explicit later
change rather than an invented release claim.

### Publish GitHub assets with repository-local authority

Verification jobs run with `contents: read`. Only the final release job receives
`contents: write`, and it publishes GitHub Release assets to the same repository
using the ephemeral `GITHUB_TOKEN`. Verification and pull-request jobs receive
no write credential. `dist/` is ignored because it is a generated,
reproducible-from-source validation output, never a committed source artifact.

### Sign and notarize standalone macOS binaries before archiving

GoReleaser's open-source cross-platform macOS notarization pipe uses Quill to
sign standalone binaries and submit them to Apple's Notary API before the
archive and checksum pipes run. This keeps the final publisher on Linux and
avoids a second platform-specific release engine. The configuration is disabled
for pull-request snapshots that do not receive secrets, while the release
workflow explicitly refuses to publish unless every Apple secret is present.

One organization-wide Developer ID Application certificate represents
LabStack's publisher identity across products. A dedicated App Store Connect
team API key named `LabStack Releases` authenticates notarization; individual
keys cannot use `notarytool`. The password-protected PKCS#12 certificate, its
password, the API private key, key ID, and issuer ID are stored as Onebox
Actions secrets. One identity avoids Apple's five-certificate ceiling and can
be reused by another LabStack product when that product adds a release workflow.

### Publish Homebrew and Scoop metadata from one scoped package token

GoReleaser generates `Casks/onebox.rb` from the signed macOS archives and
`onebox.json` from the Windows archives. The final job publishes them to
`labstack/homebrew-tap` and `labstack/scoop-bucket`. A single fine-grained GitHub
token has repository Contents write access only to those two metadata
repositories and is exposed only to the final release job. GitHub Releases
continue to use the repository's ephemeral `GITHUB_TOKEN`.

## Risks / Trade-offs

- **The calendar year is the semantic major** → Onebox is distributed as an
  application, and documentation does not advertise its CalVer tags as an
  importable Go module contract.
- **CalVer tags are not usable as normal Go module releases** → Documentation
  lists archives and packages as supported installation paths and does not
  advertise `go install`.
- **Cross-compilation cannot prove native startup** → A three-OS smoke matrix
  executes the CLI in addition to the six-target snapshot build.
- **Two releases in flight cannot be ordered** → GitHub orders queued runs by
  when they start waiting, not by tag age, so release creation refuses while the
  previous release run is still queued or running. It asks whether that run has
  finished, not whether it published: a run that failed before publication is
  the case repaired under the next revision, and waiting for a release it will
  never create would block that repair forever.
- **A tag is immutable even if publication fails** → Pull requests build the
  complete snapshot first; transient workflow failures can be rerun, while a
  source/config defect is fixed and published under the next revision without
  moving the failed tag. The rerun boundary is the GitHub Release itself: it is
  created before the Cask and the Scoop manifest, so a failure up to that point
  is rerunnable, and a failure after it leaves the release published with stale
  package metadata that the next revision repairs. A rerun of an already
  published tag is refused rather than allowed to half-republish.
- **Apple credentials are long-lived publisher authority** → Keep them in
  release-only Actions secrets, never expose them to pull-request jobs, and
  rotate the certificate and team key deliberately.
- **Homebrew and Scoop require cross-repository write authority** → Use one
  fine-grained token limited to the two metadata repositories, expose it only
  to the final release job, and fail the release rather than silently omit a
  channel.
- **Checksums do not prove publisher identity on non-macOS platforms** → macOS
  uses Developer ID and notarization now; cross-platform attestations remain an
  explicit follow-up and documentation does not overstate checksum guarantees.
- **Private GitHub Releases are not a public distribution channel** → The
  pipeline is prepared and testable while private; public package catalogs are
  enabled only after repository visibility and credential policy are decided.

## Migration Plan

1. Change the parser, release script, tests, schema examples, and documentation
   to the new grammar in one commit series; no public Onebox release tags exist
   to migrate.
2. Add and validate the GoReleaser configuration and callable CI/smoke jobs in a
   pull request without pushing a release tag.
3. Create the public `labstack/homebrew-tap` and `labstack/scoop-bucket`
   repositories with `main` branches and configure a package-metadata token with
   contents write access only to those repositories.
4. Create the shared LabStack Developer ID Application certificate and App
   Store Connect team key, store them as Onebox Actions secrets, and validate a
   signed/notarized snapshot without publishing.
5. Merge the verified change, update local `main`, and run `just release` to
   create the first `vYYYY.M.0` tag.
6. If the change must be rolled back before the first tag, revert the pull
   request. After a tag exists, never move or delete it; correct defects under a
   later monthly revision.
