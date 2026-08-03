## 1. Coverage audit before any code

- [x] 1.1 Verify the coverage table in `specs/project-schema/spec.md` against `main:internal/config/schema.cue` field by field, and record any fact with no home as a contract defect to resolve before implementation.
- [x] 1.2 Draft each of the four adopting projects under the field model on paper, including monk's `local` hooks, its `contains` and `advisory` verification, and its ordered env files; resolve every gap in the contract, not in the draft.
- [x] 1.3 Draft the project that declined the previous contract, including its four hostnames and its non-HTTP entrypoint, and resolve any gap the same way.

## 2. Normalized model foundation

- [x] 2.1 Define the normalized model from the field model — workloads with a role and exactly one source, routes, services, environments with policy and overrides, and a per-field origin record.
- [ ] 2.2 Add origin tracking (`override`, `explicit`, `shorthand`, `derived`, `default`) to every field, asserting in tests that none reaches the model without one.
- [x] 2.3 Reject a project declaring the withdrawn `components` block with an error naming the block and the authoring guide.
- [x] 2.4 Reject absent, malformed, and unsupported schema identities before validation, target contact, or generation.

## 3. CUE contract and validation

- [x] 3.1 Port `schema.cue` from this change directory into `internal/config`, unchanged in meaning, and wire it to the loader.
- [x] 3.2 Implement the conformance corpus in `conformance.md` as table-driven tests, and treat any divergence from the recorded expectation as a defect in the implementation, not the corpus.
- [ ] 3.3 Implement the workload source disjunction, the role enum with job-only `run` and required `data_effect`, and refusal of job-only fields on other roles.
- [ ] 3.4 Assert every scalar-or-object form produces canonical output identical to its object equivalent.
- [ ] 3.5 Implement registries, notifications, and secrets as named maps rather than singletons.
- [ ] 3.6 Accept and ignore `x-` keys wherever a mapping is accepted, asserting the generated runtime is unaffected by their presence.
- [ ] 3.7 Extend the error rewording layer to report every violation with its source location, never leaking the validation language, with a correction hint for a near-miss name.
- [ ] 3.8 Reject the internal validation language as an authoring input and remove that load path.
- [ ] 3.9 Fixture corpus: minimum project, every shorthand, every growable field in both forms, each identifier rule, unknown field, near-miss name, unknown enum, multiple violations, both source conflicts, a job missing `data_effect`, and an `x-` annotation.

## 4. Identifiers and paths

- [x] 4.1 Implement the identifier grammar, the `ob-` prefix reservation, the reserved words, and refusal of underscore.
- [x] 4.2 Implement refusal when the declared application identifier disagrees with the one recorded on the target.
- [x] 4.3 Implement the three path kinds: repository paths resolved against the project file and contained within the repository, absolute target paths, and request paths.
- [x] 4.4 Test that loading the same project from two working directories produces identical canonical forms.

## 5. Shorthand, overrides, defaults, and env files

- [x] 5.1 Implement scalar-to-object and top-level-to-workload expansion, rejecting a project declaring both and naming both locations.
- [x] 5.2 Implement environment overrides over the closed set, with scalar and list replacement, key-by-key mapping merge, null-removes-key, and refusal of unlisted fields and undeclared targets.
- [x] 5.3 Implement the precedence chain — override, explicit, shorthand, derived, default — with a test asserting an override beats an explicit project value.
- [x] 5.4 Implement env-file ordering with later-wins, interpolation availability, projection into application and worker workloads, and failure on a missing file.
- [ ] 5.5 Implement preflight key checks for required-and-non-empty and present.
- [ ] 5.6 Determinism test: repeated normalization of the same text yields byte-identical canonical output including map ordering.
- [ ] 5.7 Make the canonical form printable with per-field origins, with golden tests over the corpus.

## 6. Routes

- [x] 6.1 Implement the route object with domain, path, port, protocol, and TLS mode, and the scalar domain-and-port shorthand expanding to one HTTP route at `/`.
- [ ] 6.2 Reject two workloads in one environment claiming the same domain and path, naming both.
- [ ] 6.3 Test a multi-route workload and a non-HTTP route end to end through canonical form and generated routing labels.

## 7. Local generation

- [x] 7.1 Generate workload services for image-sourced, build-sourced, and Compose-sourced workloads, failing for a build-sourced workload with no resolved image and naming the interim mechanism.
- [x] 7.2 Implement the exact overlay — `ob-ingress` appended to existing networks, the three `ob.` labels, and the `traefik.` keys derived per route — asserting no other key is added, removed, or modified.
- [x] 7.3 Fail on an overlay conflict naming the key and the file: ingress network already attached, an `ob.` label, a `traefik.` label with a route, `network_mode`, or `container_name` when the rollout is rolling or replicas exceed one.
- [ ] 7.4 Preserve `container_name` on a single-replica recreate workload, and cover monk's worker as the fixture.
- [ ] 7.5 Attach the environment's configured `proxy.network` rather than a fixed name, and add neither routing labels nor a network when the proxy is disabled; reject a route declared under a disabled proxy.
- [ ] 7.6 Generate networks, volumes, and routing from the normalized model.
- [ ] 7.7 Assert generation opens no target connection on any path, success or failure.
- [ ] 7.8 Determinism and purity tests under a harness that fails on any undeclared clock, entropy, or environment input.
- [ ] 7.9 Assert a non-runtime-affecting change — an `x-` key, an inert service declaration — leaves the runtime and digest unchanged.
- [ ] 7.10 Assert a service declaration emits no container, volume, or network.

## 8. Naming and layout

- [x] 8.1 Implement underscore-joined derivation for every generated name, including application-scoped container, router, and proxy service names.
- [x] 8.2 Property test asserting injectivity: no two distinct identifier tuples, including hyphenated ones, derive the same name.
- [x] 8.3 Golden test pinning every derived name for a reference project, so a change that would rename an existing volume fails loudly.
- [ ] 8.4 Refuse at validation any derived name exceeding the container runtime's limit, naming the identifiers and the limit; assert no name is ever truncated.
- [ ] 8.5 Assert every derived name is application-scoped, including the transient rollout name, and that all of them are covered by the preflight collision check.
- [ ] 8.6 Implement the remote layout with `/var/lib/ob` as the default base path, configurable per environment with the project value as fallback, reported in observation and bound into the plan.
- [ ] 8.7 Reserve the names a declared service would derive without creating any resource.

## 9. Target preflight

- [x] 9.1 Implement preflight as a phase distinct from generation: connect, check, change nothing, fail before the first mutating command.
- [x] 9.2 Detect collisions against foreign resources by ownership label, including reserved service names, refusing rather than adopting.
- [x] 9.3 Implement the privilege check for base-path creation and container-runtime access, with the missing privilege and its remedy.
- [x] 9.4 Make generation and preflight failures distinguishable in both human and structured output.

## 10. Rendering and ejection

- [x] 10.1 Render the complete runtime without contacting a target or mutating state, with secrets by reference only.
- [ ] 10.2 Assert the rendered runtime is byte-identical to the runtime a plan binds for the same inputs.
- [x] 10.3 Implement ejection to the default destination beside the project file or an explicit one, refusing an existing path without an explicit overwrite, and stripping the overlay from the written file.
- [ ] 10.4 Assert generation succeeds immediately after ejection, proving the written file carries none of the keys the overlay refuses.
- [ ] 10.5 Make ejection crash-safe: write and atomically rename the runtime before rewriting the project, and make re-running after an interruption either complete or refuse with the reason.
- [ ] 10.6 Assert ejected services are used as authored and never regenerated or re-adopted.
- [ ] 10.7 Redaction tests over rendered and ejected output covering env files, secret references, and interpolated values.

## 11. Plan binding

- [x] 11.1 Add the generated runtime digest and the resolved base path to the executable plan binding.
- [ ] 11.2 Regenerate from the plan's own inputs at execution and refuse a digest mismatch before any mutation, directing the operator to re-plan.
- [ ] 11.3 Fault tests: edited plan, edited referenced Compose file, changed resolved image, relocated base path, and generator behavior change — each refused before mutation with a typed error.

## 12. Schema publication and agent surface

- [ ] 12.1 Export the JSON Schema from the CUE source at build time and embed it in the binary, with a test that both accept and reject the same corpus.
- [ ] 12.2 Add the command that writes the embedded schema to a repository path.
- [ ] 12.3 Make scaffolding emit the `yaml-language-server` reference comment on the project's first line, and test that an editor resolves it.
- [x] 12.4 Define the typed error-code enumeration and the structured envelope identity in the schema guide before any of it is emitted, attaching a resolving command to every failure.
- [ ] 12.5 Assert over the fixture corpus that no failure path emits an error code outside the enumeration.
- [ ] 12.6 Add versioned structured output to validation, configuration printing, rendering, and ejection, asserting diagnostics never reach the structured stream and no plaintext secret appears in it.
- [ ] 12.7 Assert idempotence: rendering and validation repeated on unchanged inputs produce identical output and change nothing.

## 13. Conversion and cutover

- [ ] 13.1 Convert the four adopting projects by hand and verify every fact from the 1.1 coverage table survived, converting each already-running data service to a workload rather than an inert service declaration.
- [ ] 13.2 Compare each converted project's generated runtime against what it runs today and resolve every difference.
- [ ] 13.3 Express the project that declined the previous contract; if it cannot be expressed, stop and revise the contract.
- [ ] 13.4 Redeploy the four projects one at a time, most tolerant first, confirming health and rollback at each step.

## 14. Documentation and archive

- [ ] 14.1 Rewrite the schema guide for the declarative contract, describing implemented behavior only, including the field model, roles, routes, overrides, extension keys, the overlay, the layout, path resolution, and ejection.
- [ ] 14.2 Document the evolution rules, including that a scalar form once accepted is accepted permanently.
- [ ] 14.3 Update `README.md` and `docs/README.md` with the breaking change and the conversion requirement.
- [ ] 14.4 Update `docs/product.md` to state the ownership boundary as product direction, without claiming unimplemented capability.
- [ ] 14.5 Re-base the active `managed-service-operation-contract` change onto this contract, replacing its MCP-facing agent interface with the CLI surface and its provider-disabled framing with the service tiers, and carrying the reserved service names forward.
- [ ] 14.6 Run the full test suite, the race detector, the Docker-gated end-to-end suite, `just diagrams-check`, and `openspec validate --all --strict`, then archive.
