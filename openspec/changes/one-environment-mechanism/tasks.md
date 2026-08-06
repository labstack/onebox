## 1. Contract

- [x] 1.1 Growth requirement gains a pre-release gate that expires at first release.
- [x] 1.2 `env_files` entries gain the object and encrypted forms.
- [x] 1.3 Three scopes plus the workload override; empty list distinct from absent.
- [x] 1.4 Role default; source never a factor.
- [x] 1.5 Precedence stated as the runtime's; refusal for connection variables at both layers.
- [x] 1.6 Decrypted entries staged with the release and persisting.
- [x] 1.7 Overlay row and ejection strip in `runtime-generation`.
- [x] 1.8 `environments.<name>.env_files` sits directly on the environment,
      matching `base_path`, which already establishes that pattern for a
      project-level default an environment can restate. An environment-scoped
      `runtime` block would introduce a second place environments carry runtime
      settings for one field's benefit.
- [x] 1.9 The provider set keeps `sops` and `age`, the set the withdrawn
      `secrets` block accepted. Absorbing a field is not a licence to narrow it.

## 2. Implement

- [x] 2.1 Entry decoding: scalar, object, encrypted object; closed provider set.
- [x] 2.2 One resolver: override, workload, environment, project; empty distinct
      from absent; origin recorded for the canonical form.
- [x] 2.3 Role default applied on the compose path as well as the image path.
      Blocked on 2.7: the overlay is where a compose-sourced workload's
      `env_file` would be written, so this lands with the projection.
- [x] 2.4 Staging decrypts each encrypted entry to its own file in the release.
- [x] 2.5 Refuse an inline `env` and a referenced `environment` key that claims a
      connection variable.
- [x] 2.6 Refuse `secrets` with the directed message.
- [x] 2.7 Overlay projection and the matching ejection strip.

### Landed so far

Entry decoding at all four scopes including overrides, the resolver, the role
gate, the withdrawn block's refusal, per-entry staging paths, and the
target-side `--env-file` list. Verified by hand across a two-environment
project: an application and a job take the environment's list, a workload
declaring the empty list receives nothing, and a daemon receives nothing until
an environment override gives it one.

Two defects found by that check rather than by the suite. Overrides were not
expanded, so the scalar form worked at every scope except the one an
environment varies — the case the field exists for. And `deepCopy` marshals
with `omitempty`, which drops an explicitly empty list and returns it as
absent, collapsing the two states the contract requires be distinguishable.

## 3. Prove

- [x] 3.1 A test per scenario. The delta's scenarios are the specification;
      one without a test is a behaviour nobody checked.
- [x] 3.2 Two environments with different entries produce different runtimes and
      neither carries the other's values.
- [x] 3.3 A compose-sourced application resolves what an image-sourced one does.
- [x] 3.4 Eject then generate: no duplicated entries.
- [ ] 3.5 Redaction covers plaintext entries, not only encrypted ones.
- [x] 3.6 Re-freeze the corpus and enumerate every moved verdict in the change
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
