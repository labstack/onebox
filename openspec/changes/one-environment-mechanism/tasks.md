## 1. Contract

- [x] 1.1 Growth requirement gains a pre-release gate that expires at first release.
- [x] 1.2 `env_files` entries gain the object and encrypted forms.
- [x] 1.3 Three scopes plus the workload override; empty list distinct from absent.
- [x] 1.4 Role default; source never a factor.
- [x] 1.5 Precedence stated as the runtime's; refusal for connection variables at both layers.
- [x] 1.6 Decrypted entries staged with the release and persisting.
- [x] 1.7 Overlay row and ejection strip in `runtime-generation`.
- [ ] 1.8 Decide whether `environments.<name>.env_files` sits on the environment
      or under an environment-scoped `runtime`. The former matches `base_path`;
      the latter matches where the project's list lives. Pick before implementing.

## 2. Implement

- [ ] 2.1 Entry decoding: scalar, object, encrypted object; closed provider set.
- [ ] 2.2 One resolver: override, workload, environment, project; empty distinct
      from absent; origin recorded for the canonical form.
- [ ] 2.3 Role default applied on the compose path as well as the image path.
- [ ] 2.4 Staging decrypts each encrypted entry to its own file in the release.
- [ ] 2.5 Refuse an inline `env` and a referenced `environment` key that claims a
      connection variable.
- [ ] 2.6 Refuse `secrets` with the directed message.
- [ ] 2.7 Overlay projection and the matching ejection strip.

## 3. Prove

- [ ] 3.1 A test per scenario. The delta's scenarios are the specification;
      one without a test is a behaviour nobody checked.
- [ ] 3.2 Two environments with different entries produce different runtimes and
      neither carries the other's values.
- [ ] 3.3 A compose-sourced application resolves what an image-sourced one does.
- [ ] 3.4 Eject then generate: no duplicated entries.
- [ ] 3.5 Redaction covers plaintext entries, not only encrypted ones.
- [ ] 3.6 Re-freeze the corpus and enumerate every moved verdict in the change
      record, which the growth gate now requires.

## 4. Verify on a host

- [ ] 4.1 Staging and production selecting different entries; each host holds
      only its own values.
- [ ] 4.2 A scheduled backup job with an encrypted entry, fired by the timer
      after a reboot, uploading successfully.

## 5. Land

- [ ] 5.1 Rewrite the environment section of `docs/schema-v1.md`, including the
      connection-URL limitation on endpoint overrides and the `*_FILE` convention
      being out of model.
- [ ] 5.2 Full suite, race, Docker-gated end-to-end, `openspec validate --all
      --strict`, then archive.
