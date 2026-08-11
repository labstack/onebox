## Why

Onebox has a guarded tag publisher but no automated path from a release tag to installable, verifiable binaries. The first public release needs one reproducible distribution path for Linux, macOS, and Windows without hand-maintained platform workflows.

## What Changes

- **BREAKING** Replace the zero-padded four-digit CalVer identity (`v2026.08.1`) with the shorter, SemVer-shaped `vYY.M.SEQUENCE` identity (`v26.8.1`).
- Make a valid release tag trigger a pinned GitHub Actions workflow that verifies the tagged commit before GoReleaser publishes artifacts.
- Build `ob` for Linux, macOS, and Windows on amd64 and arm64; publish tar/zip archives, SHA-256 checksums, Linux `.deb`/`.rpm` packages, a Homebrew Cask, and a Scoop manifest.
- Sign and notarize both macOS binaries with one LabStack Developer ID Application identity and a dedicated App Store Connect team key before they enter release archives.
- Validate the GoReleaser configuration and cross-platform binaries before a real tag can publish.
- Update the release script, parser, tests, OpenSpec, schema examples, and user documentation to one release identity.
- Publish generated package metadata to dedicated `labstack/homebrew-tap` and `labstack/scoop-bucket` repositories using release credentials scoped to those repositories.
- Keep WinGet, hosted APT/RPM repositories, SBOMs, and attestations out of this first slice until their repositories, credentials, and operating policies exist.
- Treat Onebox as a distributed application rather than a versioned importable Go library; `go install ...@v26.8.1` is not a supported installation contract.

## Capabilities

### New Capabilities

- `release-distribution`: Defines guarded tag-triggered verification and the cross-platform release artifact set.

### Modified Capabilities

- `release-versioning`: Changes the canonical CalVer grammar from four-digit, zero-padded `vYEAR.MONTH.SEQUENCE` to two-digit, unpadded `vYY.M.SEQUENCE` while preserving UTC monthly sequencing and numeric ordering.

## Impact

- Affects `internal/buildinfo`, `scripts/release.sh`, release workflow tests, generated schema/docs, installation guidance, and GitHub Actions.
- Adds a pinned GoReleaser v2 configuration, Homebrew and Scoop publication targets, Apple signing/notarization credentials, and release-time secrets scoped to those duties; normal Onebox runtime behavior and target-host requirements do not change.
- Establishes no supporting-service tier and changes no host ownership, application data, backup, restore, migration, destructive-operation, upgrade, or provider-specific behavior.
- The shipped result is GitHub Release artifacts plus Homebrew and Scoop. WinGet, hosted Linux repositories, SBOMs, and supply-chain attestations remain proposed follow-ups and SHALL NOT be documented as available.
