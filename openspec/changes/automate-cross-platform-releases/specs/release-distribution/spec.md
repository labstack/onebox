## Purpose

Defines how one guarded release tag becomes tested, installable Onebox binaries,
Linux packages, a Homebrew Cask, and a Scoop package without platform-specific
manual publication.

## ADDED Requirements

### Requirement: Only an eligible tag can publish a release

The release workflow SHALL accept only a canonical Onebox CalVer tag. It SHALL
fetch complete tag and branch history and SHALL require the tagged commit to be
reachable from `origin/main`. Tag validation and main-lineage validation SHALL
complete before any artifact is published. A malformed tag or a tag from
unmerged history SHALL fail closed without creating a GitHub Release.

#### Scenario: Guarded release tag is eligible
- **WHEN** a canonical release tag points to a commit reachable from `origin/main`
- **THEN** release verification may proceed for that exact tagged commit

#### Scenario: Manually pushed malformed tag
- **WHEN** a pushed tag does not match canonical Onebox CalVer
- **THEN** the workflow exits before publishing an artifact or GitHub Release

#### Scenario: Tag points outside main
- **WHEN** a canonical-looking tag points to a commit that is not reachable from `origin/main`
- **THEN** the workflow exits before publishing an artifact or GitHub Release

### Requirement: Verification gates publication

The tagged source SHALL pass the repository verification gate, Docker
end-to-end suite, release-configuration validation, cross-platform artifact
snapshot, and native Linux, macOS, and Windows CLI smoke checks before the
publishing job can begin. A failure or cancellation in any required check SHALL
leave the tag in place and SHALL NOT publish a GitHub Release.

#### Scenario: All release checks pass
- **WHEN** every required verification job succeeds for the tagged commit
- **THEN** the publishing job may build and publish that exact source

#### Scenario: Verification fails or is cancelled
- **WHEN** any required verification job fails or is cancelled
- **THEN** no GitHub Release or release artifact is published

### Requirement: Releases contain the supported artifact matrix

Each release SHALL contain `ob` archives for Linux, macOS, and Windows on both
`amd64` and `arm64`. Linux and macOS archives SHALL use `tar.gz`; Windows
archives SHALL use ZIP and contain `ob.exe`. Each release SHALL also contain
Debian and RPM packages for Linux `amd64` and `arm64`, installing the executable
as `/usr/bin/ob`. Builds SHALL disable CGO so the declared matrix is
cross-compiled from one release source without platform C toolchains. Before
either macOS binary enters its archive, it SHALL be signed with the LabStack
Developer ID Application identity, submitted to Apple's notarization service,
and accepted. Missing or rejected signing/notarization credentials SHALL fail
the release before publication rather than produce an unsigned macOS archive.

#### Scenario: GitHub Release artifact set is complete
- **WHEN** a release is published
- **THEN** all six platform archives and all four Linux packages are present

#### Scenario: Package is installed
- **WHEN** a released Debian or RPM package is installed on its matching architecture
- **THEN** `ob` is installed at `/usr/bin/ob`

#### Scenario: macOS archive is inspected
- **WHEN** either released macOS archive is downloaded
- **THEN** its `ob` binary carries the LabStack Developer ID signature and has an accepted Apple notarization record

#### Scenario: macOS signing cannot complete
- **WHEN** signing credentials are absent or Apple refuses notarization
- **THEN** the release fails before publishing an unsigned macOS archive

### Requirement: Release artifacts carry verifiable provenance metadata

Every released binary SHALL report the exact release tag and embedded build
time through `ob version`, while Go build metadata supplies the source revision
and source time. The release SHALL include one SHA-256 checksum manifest that
covers every published platform archive and Linux package. Archive and package
names SHALL identify the Onebox version, operating system or package format,
and architecture without inspecting their contents.

#### Scenario: Released binary reports its identity
- **WHEN** a user runs `ob version` from a published archive or package
- **THEN** the command reports the exact tag used to publish that release and its build/source provenance

#### Scenario: User verifies a downloaded artifact
- **WHEN** a user computes SHA-256 for a downloaded archive or Linux package
- **THEN** the checksum manifest contains the matching artifact name and digest

### Requirement: Scoop publishes the Windows release contract

Each release SHALL generate a Scoop manifest named `onebox.json` from the
released Windows ZIP archives for `amd64` and `arm64`. The manifest SHALL contain
the matching GitHub Release URL, SHA-256 digest, and `ob.exe` binary mapping for
each architecture. The final release job SHALL publish the manifest at the root
of `labstack/scoop-bucket` only after all verification succeeds. Its credential
SHALL have no role in pull-request or normal CI jobs.

#### Scenario: Windows user installs through Scoop
- **WHEN** the bucket contains the manifest for a successfully published release
- **THEN** `scoop install labstack/onebox` installs the matching `ob.exe` release artifact

#### Scenario: Scoop credential is absent or refused
- **WHEN** the final publisher cannot update `labstack/scoop-bucket`
- **THEN** the release job fails visibly and does not claim Scoop publication succeeded

### Requirement: Homebrew publishes the macOS release contract

Each release SHALL generate a Homebrew Cask named `onebox` from the released
macOS tar archives for `amd64` and `arm64`. The Cask SHALL contain the matching
GitHub Release URL and SHA-256 digest for each architecture and SHALL install
the `ob` executable. The final release job SHALL publish the Cask to
`labstack/homebrew-tap` only after verification succeeds. Its credential SHALL
have no role in pull-request or normal CI jobs.

#### Scenario: macOS user installs through Homebrew
- **WHEN** the tap contains the Cask for a successfully published release
- **THEN** `brew install labstack/tap/onebox` installs the signed and notarized `ob` binary for the user's architecture

#### Scenario: Homebrew credential is absent or refused
- **WHEN** the final publisher cannot update `labstack/homebrew-tap`
- **THEN** the release job fails visibly and does not claim Homebrew publication succeeded

### Requirement: Initial publication has an honest channel boundary

The initial release workflow SHALL publish artifacts to the Onebox GitHub
Release, its generated Cask to the dedicated Homebrew tap, and its generated
manifest to the dedicated Scoop bucket. Documentation SHALL distinguish
downloadable `.deb` and `.rpm` files from hosted APT/RPM repositories and SHALL
NOT claim WinGet, hosted repositories, SBOMs, or attestations until those
channels or evidence are implemented and verified.

#### Scenario: User reads installation guidance
- **WHEN** the initial GitHub Release pipeline is shipped
- **THEN** documentation describes archive, local package, Homebrew, and Scoop installation and does not advertise an unavailable catalog or attestation
