## 1. Establish the equivalence harness

- [ ] 1.1 Freeze the current verdict for all 65 conformance cases and the 19 corpus projects into a golden artifact, recording accept or reject, the typed error code, and the generated runtime digest.
- [ ] 1.2 Add a test that fails when any verdict, error code, or digest differs from the frozen artifact, and run it against the unchanged tree to prove it passes today.

## 2. Decode into typed structs

- [ ] 2.1 Declare the workload structs per role, the service, environment, proxy, registry, notification, secret, verification, and runtime shapes, mirroring the existing model.
- [ ] 2.2 Decode the expanded document with `KnownFields(true)`, selecting a workload's struct by its declared role.
- [ ] 2.3 Assert an undefined field is rejected on every role, naming the field and its line, and that the failure never names the role.
- [ ] 2.4 Keep `expand()` unchanged and assert every shorthand form still normalises identically.

## 3. Move constraints into the validation table

- [ ] 3.1 Move every enum, pattern, and bound into one table keyed by field path, each entry carrying the message its violation produces.
- [ ] 3.2 Port the hostile-value tests unchanged: timezone, image reference, registry server and username, absolute path, repository path, URL path.
- [ ] 3.3 Assert the ordinary values real projects write still load, including an IANA zone, a digest-pinned image, a registry with a port, and a nested env-file path.
- [ ] 3.4 Preserve the near-miss suggestion and assert it for a typo in each role.

## 4. Apply defaults explicitly

- [ ] 4.1 Apply every declared default by explicit assignment, in one pass, after decoding.
- [ ] 4.2 Assert each default appears in the canonical form marked as derived, including the ones CUE previously dropped on optional fields.
- [ ] 4.3 Assert defaulting does not depend on whether a field was declared.

## 5. Remove CUE

- [ ] 5.1 Delete `internal/app/schema.cue`, the rewording layer, the path extractor, the per-role pre-validation pass, and the `cuelang.org/go` dependency.
- [ ] 5.2 Run the equivalence harness and require an identical result for every case, every project, and every digest.
- [ ] 5.3 Confirm the loader's 23 typed error codes are unchanged in identity and in the command each names as the resolution.

## 6. Publish the schema

- [ ] 6.1 Generate a JSON Schema from the struct declarations and the validation table, and embed it in the binary.
- [ ] 6.2 Add the command that writes the embedded schema to a repository path.
- [ ] 6.3 Gate the published schema against the conformance corpus, requiring it to accept and reject exactly what the loader does, and fail the build on divergence.
- [ ] 6.4 Make scaffolding emit the `yaml-language-server` reference on the project's first line, and verify an editor resolves it.

## 7. Documentation and archive

- [ ] 7.1 Update `openspec/config.yaml` to describe how the contract is enforced, removing the statement that CUE provides the schema.
- [ ] 7.2 Update the schema guide to describe the published JSON Schema and how to reference it.
- [ ] 7.3 Record in the change that the contract did not move, citing the corpus result.
- [ ] 7.4 Run the full test suite, the race detector, the Docker-gated end-to-end suite, and `openspec validate --all --strict`, then archive.
