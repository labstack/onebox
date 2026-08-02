## 1. Version contract

- [x] 1.1 Enforce minimum versions through a canonical
  `vYEAR.MONTH.SEQUENCE` parser and numeric ordering that distinguishes invalid
  policy from an unreleased runner.
- [x] 1.2 Add table-driven runner-policy tests for year, month, sequence,
  malformed minimum, malformed runner, and development-build cases.

## 2. Build and release workflow

- [x] 2.1 Derive the default linked build version from Git description while
  retaining explicit `OB_VERSION` injection and a non-release fallback.
- [x] 2.2 Add a guarded monthly CalVer release recipe that checks clean,
  up-to-date `main`, runs repository checks, filters valid tags, publishes the
  next sequence, and removes its local tag after publication failure.
- [x] 2.3 Verify build provenance and inspect the rendered release recipe
  without creating or publishing a tag.

## 3. Documentation and review

- [x] 3.1 Update current public build and schema guides with canonical CalVer
  examples, ordering, and the development-runner compatibility boundary.
- [x] 3.2 Run Go tests, static analysis, vulnerability analysis, documentation
  link checks, strict validation of all OpenSpec artifacts, and a public-tree
  terminology scan.
- [x] 3.3 Review the implementation against every release-versioning scenario,
  record remaining publication blockers, and keep actual tag creation outside
  this change.
