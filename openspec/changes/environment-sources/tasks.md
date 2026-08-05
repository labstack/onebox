## 0. Blocking — a second review says this cannot be locked in yet

Two independent reviews against the third-party corpus and published upstream
deployments. These five block; the rest of this file assumes they are settled.

- [ ] 0.0 **Decide how this change ships against the growth rule.** The parent
      contract says it grows additively and SHALL NOT change the meaning of an
      accepted project. This change does: `env_files: []` flips from "absent" to
      "none", and jobs and compose-sourced workloads begin receiving values they
      did not. Three honest options — amend the growth requirement with an
      explicit pre-release clause, ship under a new identity, or drop the
      meaning changes. Being quietly inconsistent with it is not one. This is a
      product decision, not an editorial one.
- [ ] 0.1 **Write the runtime-generation delta.** That contract says the overlay
      onto a Compose-referenced workload adds a network and two label sets "and
      SHALL modify nothing else". Projecting sources into a compose-sourced
      application adds `env_file` to that service, which the table forbids. The
      marquee fix is unimplementable until the overlay contract permits it, and
      the ordering against a referenced service's own `env_file` list needs
      stating.
- [ ] 0.2 **Fix the base of the precedence stack.** Verified against the
      container runtime: `environment:` beats `env_file:`. A referenced
      service's `environment:` therefore outranks both resolved sources and
      connections, inverting levels 1<2 and 1<3 — and Immich's and authentik's
      published compose configure their databases exactly that way. Extend the
      collision refusal already applied to inline `env`, or restate level 1 as
      the referenced file's `env_file` entries only.
- [ ] 0.3 **Specify syntax, not only behaviour.** Where a source is declared,
      its shape, the field a scope's list uses, and what names `env_files`
      shorthand produces so an override can reference them. The override table
      already commits to the name `sources` while 1.1 calls it undecided; close
      that either way.
- [ ] 0.4 **Decide `secrets`.** Leaving the second mechanism accepted and
      undefined reproduces the absence this change diagnoses. The reading that
      fits both the model and what authors plainly meant: the map's keys are
      environment names, so `secrets: {production: file}` is shorthand for that
      environment's list containing that source — which also kills the
      motivating defect at its root.

## 0a. Findings from review still open

- [ ] 0.1 Define the `sources` declaration shape in the contract: where a source
      is declared, what fields it has, and how a scope's list refers to one. The
      delta specifies behaviour and not syntax, and a contract whose first
      requirement is machine-checkable closedness cannot leave the shape to the
      implementation.
- [ ] 0.2 Decide the fate of `secrets` as a named map. `env_files` is retained as
      shorthand for plaintext sources; `secrets` has no equivalent reading,
      because a map invites the selection this change removes.
- [ ] 0.3 State where a decrypted source lives on the target between releases. A
      scheduled job fires from a host timer with no Onebox process alive, so
      decrypted values must persist across reboots, and nothing says so.
- [ ] 0.4 Name the build-time boundary: values a local build hook reads are not
      container environment and are not modelled here.
- [ ] 0.5 Say in the authoring guide that a job taking a third-party image should
      declare its own sources or none, since it now inherits the application's by
      default.

## 1. Settle the authoring surface

- [ ] 1.1 Name the field. `env_files` describes plaintext files and `secrets`
      describes encrypted ones; the model needs one name covering both at
      project, environment and workload scope. A scalar form accepted once is
      accepted forever, so decide before implementing.
- [ ] 1.2 Decide how a source declares its kind — inferred from a `provider`,
      or stated. Inference is fewer characters and one more thing that can be
      wrong silently.
- [ ] 1.3 Decide the migration for the two existing fields. There are no users,
      so replacement is available and is the honest option if the model is
      better.

## 2. Implement resolution

- [ ] 2.1 One resolver returning a workload's ordered sources from workload,
      environment and project scope, with the empty list distinct from absent.
- [ ] 2.2 Refuse ambiguous selection, naming the declared sources.
- [ ] 2.3 Apply the default by role, and make it independent of the workload's
      source — the compose path must take the same branch as the image path.
- [ ] 2.4 Append managed-service connections after every declared source.

## 3. Prove the corner cases

- [ ] 3.1 One test per row of the table in `design.md`. That table is the
      specification of this change's behaviour; a row without a test is a
      behaviour nobody checked.
- [ ] 3.2 Two environments selecting different sources produce different
      rendered runtimes, and neither carries the other's values.
- [ ] 3.3 A compose-sourced application receives exactly what an image-sourced
      application receives.
- [ ] 3.4 Ambiguity is refused rather than resolved, and the failure names every
      candidate.
- [ ] 3.5 Extend the redaction tests to cover plaintext sources, which are
      currently only covered for encrypted ones.
- [ ] 3.6 Re-freeze corpus verdicts and read the diff: only projects with a
      job, a compose-sourced workload, or multiple sources may move.

## 4. Verify on a host

- [ ] 4.1 Deploy a project where staging and production select different
      sources, and confirm each host holds only its own values. This is the
      defect that motivated the change and it is not provable in a unit test.
- [ ] 4.2 Deploy the scheduled backup job with credentials from an encrypted
      source and confirm the upload succeeds.

## 5. Land

- [ ] 5.1 Rewrite the environment section of `docs/schema-v1.md` around the
      model rather than around the two fields.
- [ ] 5.2 Full suite, race detector, Docker-gated end-to-end suite,
      `openspec validate --all --strict`, then archive.
