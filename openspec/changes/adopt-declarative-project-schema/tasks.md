## 1. Coverage audit before any code

- [ ] 1.1 Enumerate every operational fact the withdrawn classifier contract could express, from `internal/config/schema.cue` and the existing projects, and record it as the coverage checklist this change is measured against.
- [ ] 1.2 Draft each of the four adopting projects under the new contract on paper and record every fact with no home; treat each as a contract defect to resolve before implementation starts.
- [ ] 1.3 Draft the project that declined the previous contract, including its multiple hostnames and its non-HTTP entrypoint, and resolve any gap the same way.

## 2. Normalized model foundation

- [ ] 2.1 Define the normalized project model — workloads with exactly one source, service declarations, routing, environment policy, and a per-field origin record — as the single shape the loader produces.
- [ ] 2.2 Add origin tracking (`explicit`, `override`, `shorthand`, `derived`, `default`) to every normalized field, and assert in tests that no field reaches the model without an origin.
- [ ] 2.3 Reject a project declaring the withdrawn `components` block with an error naming the block and the authoring guide, not a generic unknown-field error.
- [ ] 2.4 Reject absent, malformed, and unsupported schema identities before validation, target contact, or generation.

## 3. CUE contract and validation

- [ ] 3.1 Write the schema as a closed definition covering the full coverage checklist from 1.1, with the workload source disjunction, the service scalar-or-object disjunction, and the health path-or-exec disjunction.
- [ ] 3.2 Make every field identified as growable accept both a scalar and an object form, and assert in tests that the two produce identical canonical output.
- [ ] 3.3 Accept and ignore `x-` keys wherever a mapping is accepted, carrying them through normalization unchanged, and assert the generated runtime is unaffected by their presence.
- [ ] 3.4 Extend the error rewording layer to report every violation with its source location, never leaking the validation language, and to emit a correction hint for a near-miss field name.
- [ ] 3.5 Reject the internal validation language as an authoring input and remove that load path.
- [ ] 3.6 Export a JSON Schema from the same CUE source at build time, with a test that round-trips the valid and invalid fixture corpus through both and fails when they disagree.
- [ ] 3.7 Fixture corpus: minimum project, every shorthand form, every growable field in both forms, unknown field, near-miss name, unknown enum value, multiple simultaneous violations, both workload-source conflicts, and an `x-` annotation.

## 4. Shorthand, overrides, and defaults

- [ ] 4.1 Implement scalar-to-object and top-level-to-workload expansion, rejecting a project that declares top-level workload fields alongside a workload block and naming both locations.
- [ ] 4.2 Implement environment overrides over the enumerated field set, rejecting an unlisted field with a message listing what is overridable and rejecting an override naming an undeclared workload or service.
- [ ] 4.3 Implement the documented precedence chain — explicit, override, shorthand, derived, default — recording origin at each step and taking no environmental input.
- [ ] 4.4 Determinism test asserting repeated normalization of the same text yields byte-identical canonical output, including map ordering.
- [ ] 4.5 Make the canonical form printable with per-field origins, and add golden tests over the fixture corpus.

## 5. Identifiers, layout, and naming

- [ ] 5.1 Specify and implement the identifier rules: character set, length, reserved names including the host namespace, and refusal when the declared application identifier disagrees with the one recorded on the target.
- [ ] 5.2 Implement the documented remote layout with its default base path, and promote the base path to a project setting reported in observation and bound into plans.
- [ ] 5.3 Implement deterministic name derivation for generated projects, networks, and volumes, with length validation and collision-resistant truncation.
- [ ] 5.4 Add a golden test pinning every derived name for a reference project, so a later change that would rename an existing volume fails loudly rather than silently.
- [ ] 5.5 Implement the privilege precheck for the configured account — base-path creation and container-runtime access — failing before mutation with the missing privilege and the remedy.
- [ ] 5.6 Implement pre-generation collision detection against target resources Onebox does not own by label, failing rather than adopting or overwriting.

## 6. Runtime generation

- [ ] 6.1 Generate workload services for image-sourced, build-sourced, and Compose-sourced workloads, failing closed for a build-sourced workload with no resolved image and naming the interim resolution mechanism.
- [ ] 6.2 Implement the enumerated overlay as a closed set, failing with a named conflict when the referenced service already sets an overlaid key, and assert no key outside the set is touched.
- [ ] 6.3 Generate networks, volumes, and routing from declared domains, ports, and protocols, including multiple domains per workload and a non-HTTP entrypoint.
- [ ] 6.4 Generate supporting services into a project separate from the application's, and assert an application rollback and a teardown leave their containers and volumes intact.
- [ ] 6.5 Assert a service declaration emits no container, volume, or network, and that a workload depending on one reports it as not managed by Onebox.
- [ ] 6.6 Determinism and purity tests: identical inputs produce identical bytes and digests, under a harness that fails on any undeclared clock, entropy, or environment input.
- [ ] 6.7 Fail-closed tests: every generation failure path leaves no local or staged artifact and attempts no connection.

## 7. Rendering and ejection

- [ ] 7.1 Render the complete generated runtime without contacting a target or mutating state, with secret values represented by reference only.
- [ ] 7.2 Assert the rendered runtime is byte-identical to the runtime a plan binds for the same inputs.
- [ ] 7.3 Implement ejection: write the runtime, repoint affected workloads at it, refuse to overwrite without an explicit request, and leave inputs unchanged on failure.
- [ ] 7.4 Assert ejected services are used as authored on subsequent generation and are never regenerated or re-adopted.
- [ ] 7.5 Redaction tests over rendered and ejected output covering environment files, secret references, and interpolated values.

## 8. Plan binding

- [ ] 8.1 Add the generated runtime digest and the configured base path to the executable plan binding.
- [ ] 8.2 Regenerate the runtime from a plan's own inputs at execution and refuse a digest mismatch before any target mutation, directing the operator to re-plan.
- [ ] 8.3 Fault tests: edited plan, edited referenced Compose file, changed resolved image, relocated base path, and generator behavior change — each refused before mutation with a typed error.

## 9. Agent-operable CLI surface

- [ ] 9.1 Add versioned structured output to validation, configuration printing, rendering, and ejection.
- [ ] 9.2 Define the typed error-code set and attach a resolving command to every failure, asserting coverage over the fixture corpus so no failure path emits an untyped error.
- [ ] 9.3 Assert diagnostics never reach the structured stream and structured output carries no plaintext secret values.
- [ ] 9.4 Assert idempotence: rendering and validation repeated on unchanged inputs produce identical output and change nothing.

## 10. Conversion and cutover

- [ ] 10.1 Convert the four adopting projects by hand and verify every fact from the 1.1 checklist survived.
- [ ] 10.2 Compare each converted project's generated runtime against what it runs today and resolve every difference before it deploys.
- [ ] 10.3 Express the project that declined the previous contract; if it cannot be expressed, stop and revise the contract.
- [ ] 10.4 Redeploy the four projects one at a time, most tolerant first, confirming health and rollback at each step.

## 11. Documentation and archive

- [ ] 11.1 Rewrite the schema guide for the declarative contract, describing implemented behavior only, including the ownership boundary, shorthand, growable forms, overrides, extension keys, the overlay set, the layout, and ejection.
- [ ] 11.2 Document the evolution rules that keep the contract additive, so the next contributor knows what may and may not change under this identity.
- [ ] 11.3 Update `README.md` and `docs/README.md` with the breaking change and the conversion requirement.
- [ ] 11.4 Update `docs/product.md` to state the ownership boundary as product direction, without claiming unimplemented capability.
- [ ] 11.5 Re-base the active `managed-service-operation-contract` change onto this contract, replacing its MCP-facing agent interface with the CLI surface and its provider-disabled framing with the service tiers.
- [ ] 11.6 Run the full test suite, the race detector, the Docker-gated end-to-end suite, `just diagrams-check`, and `openspec validate --all --strict`, then archive.
