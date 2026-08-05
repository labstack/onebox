## 0. Findings from review still open

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
