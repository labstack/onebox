## 1. Normalized model foundation

- [ ] 1.1 Define the normalized project model — workloads with exactly one source, service declarations, environment policy, and a per-field origin record — as the single shape both loaders produce, with no dependency on the authoring version.
- [ ] 1.2 Add a schema-identity resolver that selects a loader from the declared identity and rejects absent, malformed, and unsupported identities before validation, target contact, or generation.
- [ ] 1.3 Add origin tracking (`explicit`, `shorthand`, `derived`, `default`) to every normalized field, and assert in tests that no field reaches the model without an origin.
- [ ] 1.4 Table-driven tests for identity selection covering v1, v2, absent, malformed, and future-version identities, asserting no connection is attempted on rejection.

## 2. CUE contract and validation

- [ ] 2.1 Write the v2 CUE schema as a closed definition: required fields, the workload source disjunction, the service scalar-or-object disjunction, the health path-or-exec disjunction, and scalar constraints.
- [ ] 2.2 Extend the error rewording layer to report every violation with its source location, never leaking the validation language, and to emit a correction hint when a rejected name is close to an accepted name valid in that position.
- [ ] 2.3 Reject the internal validation language as an authoring input with an error naming the conversion command; remove the CUE authoring load path.
- [ ] 2.4 Export a JSON Schema from the same CUE source at build time and add a test that round-trips the valid and invalid fixture corpus through both, failing when they disagree.
- [ ] 2.5 Fixture corpus for validation: minimum project, every shorthand form, unknown field, near-miss field name, unknown enum value, multiple simultaneous violations, and both workload-source conflicts.

## 3. Shorthand expansion and defaults

- [ ] 3.1 Implement scalar-to-object service expansion and top-level-to-workload expansion, rejecting a project that declares top-level workload fields alongside a workload block and naming both locations.
- [ ] 3.2 Implement the documented default precedence chain — explicit, shorthand, derived, contract default — recording origin at each step and taking no environmental input.
- [ ] 3.3 Add a determinism test asserting that normalizing the same project text repeatedly yields byte-identical canonical output, including map ordering.
- [ ] 3.4 Make the canonical form printable through the existing configuration command, including per-field origins, and add golden tests over the fixture corpus.

## 4. Compose reference and merge boundary

- [ ] 4.1 Implement the bounded `file#service` workload reference, failing validation when the file or the named service is absent and naming both.
- [ ] 4.2 Reject a Compose reference or raw runtime override on a service declaration, with an error stating that a user-authored data service must be declared as a workload.
- [ ] 4.3 Implement the enumerated overlay — ingress attachment, release identity, domain-derived routing, rolling container naming — as a closed set, and fail generation with a named conflict when the referenced service already sets an overlaid key.
- [ ] 4.4 Tests for the overlay: preserved settings the contract cannot express, each overlaid key conflicting individually, and an assertion that no key outside the enumerated set is written.

## 5. Runtime generation

- [ ] 5.1 Implement deterministic name derivation for generated projects, networks, and volumes, with length validation and collision-resistant truncation.
- [ ] 5.2 Implement pre-generation collision detection against target resources Onebox does not own by label, failing rather than adopting or overwriting.
- [ ] 5.3 Generate workload services for image-sourced, build-sourced, and Compose-sourced workloads, failing closed for a build-sourced workload with no resolved image and naming the interim resolution mechanism.
- [ ] 5.4 Generate networks, volumes, and domain-derived proxy routing from the normalized model.
- [ ] 5.5 Assert that a service declaration emits no container, volume, or network, and that a workload depending on a declared service reports it as not managed by Onebox.
- [ ] 5.6 Determinism and purity tests: identical inputs produce identical bytes and digests; the generator is exercised under a clock, entropy, and environment harness that fails on any undeclared input.
- [ ] 5.7 Fail-closed tests: every generation failure path leaves no local or staged artifact and attempts no connection.

## 6. Rendering and ejection

- [ ] 6.1 Render the complete generated runtime without contacting a target or mutating state, with secret values represented by reference only.
- [ ] 6.2 Add a test asserting the rendered runtime is byte-identical to the runtime a plan binds for the same inputs.
- [ ] 6.3 Implement ejection: write the generated runtime, rewrite the affected workloads to Compose references into it, refuse to overwrite an existing destination unless explicitly requested, and leave inputs unchanged on failure.
- [ ] 6.4 Assert that ejected services are used as authored on subsequent generation and are never regenerated or re-adopted.
- [ ] 6.5 Redaction tests over rendered and ejected output covering environment files, secret provider references, and interpolated values.

## 7. Plan binding

- [ ] 7.1 Add the generated runtime digest to the executable plan binding alongside the existing config, Compose, host-state, image, and payload commitments.
- [ ] 7.2 Regenerate the runtime from a plan's own inputs at execution and refuse a digest mismatch before any target mutation, directing the operator to re-plan.
- [ ] 7.3 Fault tests: edited plan, edited referenced Compose file, changed resolved image, and generator behavior change — each refused before mutation with a typed error.

## 8. v1 coexistence and deprecation

- [ ] 8.1 Freeze the v1 loader behind the identity resolver with behavior unchanged, and keep its existing tests as the regression suite.
- [ ] 8.2 Emit the deprecation notice on every v1 load, naming the conversion command and the release in which support ends, on the diagnostic stream only.
- [ ] 8.3 Assert that structured output remains parseable while the deprecation notice is emitted.
- [ ] 8.4 End-to-end assertion that an unmodified v1 project plans and executes identically to the pre-change binary.

## 9. Conversion

- [ ] 9.1 Convert a v1 project and its Compose file into a v2 project: applications, workers, and jobs become workloads; a data service becomes a service declaration when a driver name exists and a workload otherwise.
- [ ] 9.2 Refuse to infer persistence semantics, data effects, migration compatibility, backup posture, or destructive tolerance the v1 project did not state, reporting each as requiring a decision.
- [ ] 9.3 Report every unconvertible construct with its source location, classified as requiring a decision or preserved by Compose reference, and assert that nothing is silently discarded.
- [ ] 9.4 Keep conversion local: no target contact, no overwrite without explicit request, and inputs unchanged on failure.
- [ ] 9.5 Implement runtime comparison between a converted project and its v1 source, reporting every difference and resolving none automatically.
- [ ] 9.6 Conversion fixtures derived from the four adopting projects, including the case where a referenced Compose service sets an overlaid routing key.

## 10. Agent-operable CLI surface

- [ ] 10.1 Add versioned structured output to validation, configuration printing, rendering, ejection, and conversion.
- [ ] 10.2 Define the typed error-code set for this contract and attach a resolving command to every failure, asserting coverage over the fixture corpus so no failure path emits an untyped error.
- [ ] 10.3 Assert that diagnostics never reach the structured stream and that structured output contains no plaintext secret values.
- [ ] 10.4 Assert idempotence: rendering, conversion, and validation repeated on unchanged inputs produce identical output and change nothing.

## 11. Documentation and archive

- [ ] 11.1 Write the v2 authoring guide describing implemented behavior only, including the workload/service boundary, shorthand, the enumerated overlay, and ejection.
- [ ] 11.2 Mark `docs/schema-v1.md` deprecated, stating the window and the conversion command.
- [ ] 11.3 Update `README.md` and `docs/README.md` for v2 status, the deprecation window, and the authority of this change until archive.
- [ ] 11.4 Update `docs/product.md` to state the ownership boundary as product direction, without claiming unimplemented capability.
- [ ] 11.5 Convert the four adopting projects, resolve every reported runtime difference, and record the results.
- [ ] 11.6 Express the project that declined v1 as a v2 project; if it cannot be expressed, stop and revise the contract before proceeding.
- [ ] 11.7 Re-base the active `managed-service-operation-contract` change onto this contract, replacing its MCP-facing agent interface with the CLI surface and its provider-disabled framing with the service tiers.
- [ ] 11.8 Run the full test suite, the race detector, the Docker-gated end-to-end suite, and `openspec validate --all --strict`, then archive.
