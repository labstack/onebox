## 1. Derivation

- [ ] 1.1 Change `slotNames` to derive `<app>_<component>` for a single replica and `<app>_<component>_<n>` beyond one, taking the application identifier as an explicit argument rather than reading global state.
- [ ] 1.2 Update every call site in `internal/engine/roll.go` that renames or matches a container by derived name.
- [ ] 1.3 Scope the transient rollout name the same way, replacing the bare `<svc>-new` form in `internal/engine/roll.go`.
- [ ] 1.4 Update status and observation paths that match containers by name in `internal/engine/status.go` and `internal/engine/status_snapshot.go`.

## 2. Refusal and preflight

- [ ] 2.1 Refuse at validation a derived container name exceeding the runtime's limit, naming both identifiers and the limit.
- [ ] 2.2 Detect a foreign container holding any derived name — stable slots and the transient rollout name — by label before any container is started, stopped, or renamed, failing with the holder named.
- [ ] 2.4 Refuse an authored fixed container name rather than honouring it, consistent with the declarative-schema overlay rule.
- [ ] 2.3 Assert an existing container from this application's previous release is treated as the normal handover case, not a foreign holder.

## 3. Tests

- [ ] 3.1 Table-driven derivation tests including hyphenated identifiers, asserting no two distinct pairs derive the same name.
- [ ] 3.2 Test that two applications declaring the same component name derive different container names.
- [ ] 3.3 Test stability across consecutive releases and across a rollback.
- [ ] 3.4 Update existing tests asserting bare component names as container names.
- [ ] 3.5 Docker-gated end-to-end test deploying two applications with identically named components to one host.

## 4. Documentation and archive

- [ ] 4.1 Note in `README.md` that the first deployment after this release renames containers, and that scripts referencing bare container names must be updated.
- [ ] 4.2 Run the full test suite, the race detector, the Docker-gated suite, and `openspec validate --all --strict`, then archive.
