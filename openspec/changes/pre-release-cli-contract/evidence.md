# Verification evidence

## P0 execution truth

The compatibility baseline was deliberately re-frozen at 123 verdicts after
adding these explicit contract cases:

| Case | Previous behavior | Frozen behavior |
|---|---|---|
| rolling workload with `published_ports` | accepted until target-side rollout failure | rejected locally as `project_invalid` |
| recreate workload with `published_ports` | accepted | accepted; fixed host socket has a single owner |
| explicit `when: manual` job | deploy graph selected every job | project and release runtime remain valid, but deploy execution selects neither pre- nor post-release phase |

Corpus applications that intentionally publish fixed host ports now declare
`strategy: recreate`; this is an authored compatibility decision rather than a
validator exception. Manual scheduled jobs remain in the rendered release
runtime, so their timers target the same digest-pinned service while normal
deployment leaves them inert.

Focused gate: `go test ./internal/app ./internal/compose ./internal/engine ./internal/onebox ./cmd/ob` — 855 tests passed.

## P0 recovery truth

The release store now refuses manifest-less current and interrupted releases;
tests construct the versioned state they exercise instead of relying on
runtime migration paths. Secret rotation was faulted at prepared, partially
replaced, verifying, committing, committed, and recovering boundaries. It
converged to an all-new generation on forward resumption or an all-old
generation on recovery, retained a typed checkpoint when recovery was
incomplete, and emitted neither plaintext nor content hashes.

Focused lifecycle/fault gate:
`go test ./internal/app ./internal/release ./internal/journal ./internal/engine ./internal/onebox`
— 873 tests passed.

## P1 lean CLI consistency

Every executable leaf now has an explicit output class and allowed mode set;
the command-tree test rejects unclassified leaves, stale matrix entries, and
aliases. Finite JSON uses the single `onebox.run/cli/v1alpha1` envelope.
NDJSON assigns monotonic record sequences, preserves stdout/stderr channels,
and permits exactly one terminal record. Typed project, lifecycle, recovery,
and runtime-target causes survive conversion to safe public errors.

Focused adapter gates:

- `go test ./cmd/ob` — 70 tests passed, including explicit config ejection,
  JSON/NDJSON ownership, SOPS exit 200 no-op, exit-2 destroy cancellation,
  output-matrix closure, and Run-tier service logs/exec channel tagging.
- `go test ./internal/engine ./internal/transport` — 277 tests passed,
  including workload/service target resolution and distinct streamed channels.

## Final release gate

- `just check` passed: `go mod tidy -diff`, formatting, `go vet ./...`, the full
  Go suite across 20 packages, generated-document drift, diagram drift, Astro
  diagnostics/build, and documentation table checks.
- `golangci-lint config verify` passed; `golangci-lint run ./...` reported zero
  issues.
- `govulncheck ./...` found no reachable vulnerabilities.
- Workflow validation passed.
- `openspec validate --all --strict --no-interactive` — 5 passed, 0 failed.
- `ob-docgen --check` — 16 generated pages and 1 published schema artifact current.
- `astro check`, `astro build`, and documentation table checks — passed; 39 pages built.
- `OB_E2E=1 go test ./e2e/ -count=1 -timeout 20m` passed in 201.731s,
  covering bootstrap ownership, zero-downtime replacement, killed-runner
  resume, and failed-worker preservation of the serving release.
- Throwaway Hetzner `cpx22`, Ubuntu 24.04, Gitea: bootstrap → saved plan →
  plan-bound local confirmation → deploy → host-side HTTP verification passed
  in 108s; HTTP 200, 2 running application containers, 2 persistent volumes,
  and a serving release manifest. The cleanup trap exited 0 and the provider
  listed zero remaining `purpose=onebox-e2e` servers.
