## 1. P0 execution truth

- [x] 1.1 Reject rolling workloads with published host ports in project validation and add loader/schema regression tests proving no target access is required.
- [x] 1.2 Exclude `when: manual` jobs from deploy graphs and engine gates while retaining their release runtime and timers; add graph, execution, and scheduling tests.
- [x] 1.3 Add canonical job plan/run service operations bound to current release state, immutable image, data effect, local confirmation, backup report, and journal result; add stale-state and migration-gate tests.
- [x] 1.4 Add the `ob job plan|run` adapter and structured results, including interactive human confirmation and saved-plan/separately recorded local-confirmation paths.
- [x] 1.5 Resolve and substitute digest references for every image in the bound release runtime, including native jobs/daemons and Compose-referenced images/builds; add plan/render/failure tests.
- [x] 1.6 Deliberately re-freeze `contract-verdicts.json`, enumerate the rolling-port and manual-job verdict changes, and run the complete app/plan test gate before P0 recovery work.

## 2. P0 recovery truth

- [x] 2.1 Add a versioned, mode-protected release manifest state machine with kind, lifecycle state, operation outcome, predecessor, and atomic parser/writer tests.
- [x] 2.2 Write manifests from deploy and bootstrap and checkpoint activation transitions across verified, symlink, serving, and predecessor updates; reject manifest-less current state and add crash-boundary tests.
- [x] 2.3 Select rollback strictly through predecessor eligibility, including repeated rollback and post-activation failure; add corrupt and unknown-state tests.
- [x] 2.4 Separate retention garbage collection from rollback eligibility and protect current, predecessor-chain, and checkpoint-referenced releases; add age/evidence tests.
- [x] 2.5 Centralize abort and automatic-rollback cleanup/finalization, preserve the migration-effect gate, remove exact interrupted-release containers, and clear checkpoints only after healthy recovery; add retry, break-glass, and stale-newcomer tests.
- [x] 2.6 Compare exact encrypted-entry declaration graphs before secret mutation and return typed deploy guidance for any drift; add add/remove/reorder/provider/scope no-mutation tests.
- [x] 2.7 Implement checkpointed opaque secret generations, generation-labelled force replacement, all-old/all-new terminal recovery, and crash resumption; add changed, unchanged, partial-failure, and redaction tests.
- [x] 2.8 Run focused lifecycle and fault-injection test gates before adapter-wide work.

## 3. P1 lean CLI consistency

- [x] 3.1 Make eject and every project-reading adapter honor the global config path; add a non-default-path round-trip test.
- [x] 3.2 Resolve Onebox-run, Run-tier PostgreSQL/Redis services for logs and exec without adding credentials to arguments or envelopes; add target-kind and unknown-target tests.
- [x] 3.3 Implement the finite JSON envelope, NDJSON event/terminal records, typed-cause preservation, stdout/stderr ownership, and stable 0/1/2 exit mapping through shared helpers.
- [x] 3.4 Add the closed leaf-command output matrix covering finite operations, logs/follow, exec passthrough, editors, help, and completion; enforce it with golden tests.
- [x] 3.5 Normalize SOPS no-change editing, other benign no-ops, and typed confirmation cancellation to the common outcomes.

## 4. Documentation and release evidence

- [x] 4.1 Update README and CLI/schema documentation for job invocation, release states, secret generations, Run-tier service targeting, passthrough sensitivity, and the command/output matrix.
- [x] 4.2 Run focused package tests, the full Go suite, strict OpenSpec validation, formatting, static analysis, and the end-to-end harness; record all passing evidence before archive.

## 5. P0 pre-release contract correction

- [x] 5.1 Replace public claims of authenticated/out-of-band approval with the exact local-confirmation contract, bind every mutable confirmation field, and add source/tamper regression tests.
- [x] 5.2 Replace migration-backup facts/receipt CLI artifacts with `onebox.run/backup-report/v1alpha1`, add `plan|job plan --backup-report-out`, bind report digests in confirmation, accept `--backup-report` at deploy/job execution, and seal one internal receipt per attempt.
- [x] 5.3 Remove `backup-evidence template|create`, `--backup-evidence`, legacy schemas, command-matrix entries, resolving text, and aliases; add command-tree and no-legacy tests.
- [x] 5.4 Split generic force into exact lock, mount-detachment, and migration-gate request fields; prove `--allow-destructive-mounts` cannot break locks or bypass unsupported service majors.
- [x] 5.5 Enforce one application owner per host across bootstrap, preflight, proxy apply, service apply, deploy, and destroy; remove cross-application proxy registry/conflict behavior and add foreign-owner no-mutation tests.
- [x] 5.6 Add stable value-free secret declaration identifiers, `ob secrets list`, and exact `ob secrets edit <entry-id>` selection with single-entry convenience and ambiguity tests.
- [x] 5.7 Add diagnostic/next/resolving command roles to structured failures and remove self-resolving read-only loops from status and protection guidance.
- [x] 5.8 Require a bounded reason for `ob exec` and journal only safe command-digest invocation evidence; add success, failure, cancellation, redaction, and missing-reason tests.

## 6. Corrected documentation and release evidence

- [x] 6.1 Update README, product/security language, CLI/schema reference, guides, generated docs, and examples for local confirmation, backup reports, exact override flags, one-host ownership, secret selection, guidance roles, and audited exec.
- [x] 6.2 Re-run focused authority/backup/force/proxy/secrets/exec tests, full Go tests, vet, lint, strict OpenSpec validation, formatting, doc generation, site checks, Docker E2E, and the throwaway Hetzner lifecycle; record passing and explicitly scoped evidence before archive.

## 7. Post-activation completion

- [x] 7.1 Journal retention, schedule sync, and the post-deploy hook as individually skippable finalize steps, and record a failed operation outcome on the serving manifest when one of them fails.
- [x] 7.2 Add the evidence-gated finalize path so `ob resume` completes a deploy interrupted after activation without replaying its choreography, refusing with `finalize_refused` on any disagreement.
- [x] 7.3 Report a retention evidence refusal instead of failing an operation whose release is already serving, and stop offering a deploy that a later terminal deploy superseded for resume or abort.
