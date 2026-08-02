## Review outcome

Approved for archive after external specification review. The reviewed delta
is synchronized into the canonical `release-versioning` specification without
creating or publishing a release tag.

## Findings addressed

- Release months are explicitly UTC, and sequence evidence crosses the numeric
  `.9` to `.10` boundary.
- The minimum-runner contract is scoped to lifecycle planning and execution;
  `ob exec` remains a separately controlled break-glass adapter.
- Publication performs a real branch compare-and-swap. Executable testing
  establishes the narrower boundary: a no-op branch refspec rejects changes
  visible in the remote advertisement but misses a change injected after that
  advertisement. The selected design keeps a real metadata-only fast-forward
  in the atomic transaction so the exact lease covers that final window.
- Branch policy is an explicit dependency. The release identity must be allowed
  to fast-forward `main`; otherwise atomic publication fails closed and removes
  its attempted local tag.
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
  competing-tag race scenarios, plus branch-policy refusal.
- A characterization test injects a branch advance after remote ref
  advertisement and proves that a no-op branch refspec can publish the stale
  tag at that boundary.
- Successful harness cases prove that the release commit has the checked commit
  as its sole parent, preserves the checked tree, and is named by remote `main`
  and the release tag.
- Failed harness cases prove atomic branch/tag refusal, removal of the attempted
  local tag, preservation of local `main`, and preservation of winning remote
  state.
- Full Go tests, race-enabled tests, and Go static analysis pass.
- The release script passes Bash syntax validation.
- Sequence arithmetic is explicitly base ten. Leading-zero `.08` and `.09`
  tags remain non-canonical and are filtered before arithmetic.
- Every canonical and active OpenSpec artifact passes strict validation, and
  the OpenSpec relationship doctor reports a healthy repository.
- Git diff whitespace validation passes.

## Publication boundaries

- No Onebox release tag, release commit, artifact, or GitHub release was
  created. Release tests use only temporary local repositories.
- External specification review is complete, and the reviewed delta is
  archived into the canonical `release-versioning` specification.
- Managed PostgreSQL, Redis, and all other production providers remain disabled
  and unimplemented pending their own reviewed changes and qualification.
