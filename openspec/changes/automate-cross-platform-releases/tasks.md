## 1. CalVer Contract

- [x] 1.1 Change the buildinfo release parser and comparison tests to canonical `vYY.M.SEQUENCE`, including zero-padding, range, malformed, and numeric-order cases.
- [x] 1.2 Change `scripts/release.sh` and its hermetic workflow tests to calculate unpadded UTC months and two-digit supported years without weakening atomic publication or race handling.
- [x] 1.3 Update authored schema examples and every source documentation reference to the new release grammar, then regenerate derived schema and CLI/reference artifacts.

## 2. Release Artifact Definition

- [x] 2.1 Add a GoReleaser v2 configuration for six `ob` archives, four Linux packages, embedded build metadata, conventional names, and one SHA-256 manifest.
- [x] 2.2 Add generated release output to ignore rules and add repository tests that assert the configured target/package/checksum contract cannot silently shrink.
- [x] 2.3 Add a testable release-tag/`origin/main` lineage validator used by GitHub Actions, with accepted, malformed, and off-main cases.
- [x] 2.4 Add Scoop manifest generation for both Windows architectures and extend the release configuration contract test.
- [x] 2.5 Add macOS binary signing/notarization and Homebrew Cask generation for both macOS architectures, then extend the release configuration contract test.

## 3. Verification and Publication Workflows

- [x] 3.1 Make the existing CI workflow reusable without changing its pull-request/main behavior or read-only permission boundary.
- [x] 3.2 Add pinned GoReleaser check/snapshot validation and Linux package/archive inspection to normal CI before the clean-tree assertion.
- [x] 3.3 Add native Linux, macOS, and Windows smoke jobs that compile and execute safe `ob` version, help, and schema surfaces.
- [x] 3.4 Add a tag-triggered release workflow that calls reusable CI, validates the tag lineage, grants `contents: write` only to the final pinned GoReleaser publishing job, and passes Apple and package-repository secrets only to that job.
- [ ] 3.5 Provision public `labstack/homebrew-tap` and `labstack/scoop-bucket` repositories plus one package-metadata token scoped to those repositories, then verify their presence without exposing credential material.
- [ ] 3.6 Provision the shared LabStack Developer ID Application certificate and App Store Connect team key as Onebox Actions secrets, then make the release workflow fail closed when any required secret is absent.

## 4. User-Facing Installation Contract

- [x] 4.1 Document GitHub archive, checksum, Debian, RPM, Homebrew, and Scoop installation and verification while explicitly excluding `go install` and unavailable package-manager catalogs or attestations.
- [x] 4.2 Update documentation status so GitHub Release artifacts, Homebrew, Scoop, and macOS signing/notarization are shipped only after implementation tests pass, with WinGet, hosted repositories, SBOMs, and attestations remaining proposed.

## 5. Completion Evidence

- [x] 5.1 Run the release parser/script/validator tests and native cross-compilation checks for all six target tuples.
- [x] 5.2 Run GoReleaser `check` and a clean unsigned snapshot, verify the exact archive/package/checksum inventory, Homebrew Cask, Scoop manifest, and embedded `ob version`, and leave no untracked release output.
- [x] 5.3 Run `just check`, workflow lint, and strict OpenSpec validation; inspect the final diff for generated-document drift and unrelated changes.
- [ ] 5.4 Run a non-publishing signed/notarized snapshot with the production Apple credentials and verify both macOS binaries' Developer ID identity and accepted notarization status without exposing credential material.
