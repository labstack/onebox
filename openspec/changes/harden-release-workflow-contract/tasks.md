## 1. Contract Corrections

- [x] 1.1 Strictly validate the UTC, lifecycle-gate, and atomic publication delta before implementation.
- [x] 1.2 Correct the archived release design and review evidence so they describe the branch compare-and-swap and executable harness precisely.

## 2. Release Workflow

- [x] 2.1 Extract the guarded release implementation to `scripts/release.sh` and make `just release` a thin adapter.
- [x] 2.2 Use the UTC calendar month and a metadata-only fast-forward release commit while retaining post-check revalidation, exact branch lease, atomic branch-plus-tag publication, and local-tag cleanup on failure.

## 3. Executable Evidence

- [x] 3.1 Add a disposable bare-repository harness for first-of-month, `.9` to `.10`, and prior-month reset behavior.
- [x] 3.2 Add check-time, pre-publication `origin/main`, and competing-tag race cases that assert atomic failure, local cleanup, and preservation of winning remote state.
- [x] 3.3 Run the focused harness, full Go tests, race tests, static analysis, formatting, and release-script syntax checks.

## 4. Documentation Lifecycle

- [x] 4.1 Run strict OpenSpec validation and relationship checks for every canonical and active change.
- [x] 4.2 Record verification evidence and leave the completed delta active for external specification review without creating or publishing a release tag.
