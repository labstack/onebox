## Review status

Ready for external specification review. Keep this change active until that
review is resolved; archive it into the canonical `release-versioning`
specification only after approval.

## Findings addressed

- Release months are explicitly UTC, and sequence evidence crosses the numeric
  `.9` to `.10` boundary.
- The minimum-runner contract is scoped to lifecycle planning and execution;
  `ob exec` remains a separately controlled break-glass adapter.
- Publication performs a real branch compare-and-swap. Executable testing
  proves that a no-op branch refspec is omitted by Git; the selected design
  atomically publishes a metadata-only fast-forward release commit and its tag
  under an exact lease.
- The managed-service proposal tool is now target-read-only but locally
  stateful, with `readOnlyHint: false` and explicit non-destructive semantics.
- Managed-service crash recovery is normative across process death, journal
  reconciliation, effect skipping, incomplete evidence, and stale fencing.
- Managed runtime identities require collision-resistant truncation and
  pre-target collision refusal.
- The managed-service proposal no longer claims that no canonical capability
  exists.

## Verification evidence

- The disposable bare-repository harness passes first-of-month, `.9` to `.10`,
  prior-month reset, check-time branch race, publication-time branch race, and
  competing-tag race scenarios.
- Successful harness cases prove that the release commit has the checked commit
  as its sole parent, preserves the checked tree, and is named by remote `main`
  and the release tag.
- Failed harness cases prove atomic branch/tag refusal, removal of the attempted
  local tag, preservation of local `main`, and preservation of winning remote
  state.
- Full Go tests, race-enabled tests, and Go static analysis pass.
- The release script passes Bash syntax validation.
- Every canonical and active OpenSpec artifact passes strict validation, and
  the OpenSpec relationship doctor reports a healthy repository.
- Git diff whitespace validation passes.

## Publication boundaries

- No Onebox release tag, release commit, artifact, or GitHub release was
  created. Release tests use only temporary local repositories.
- The follow-up change remains active for review; it has not been archived into
  the canonical specification.
- Managed PostgreSQL, Redis, and all other production providers remain disabled
  and unimplemented pending their own reviewed changes and qualification.
