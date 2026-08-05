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
