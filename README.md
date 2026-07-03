# yeet

> Working codename — the release gets a searchable, non-slang name.

**Zero-downtime deploys for docker-compose apps: agentless, plan-before-apply, keep your proxy.**

An agentless, single-binary deploy engine for apps described by a docker-compose file: preview every
deploy as a rendered diff and an exact command list, then release with health-gated zero downtime —
keeping your own proxy, database, and conventions.

## Status

**M1 implemented — plan/apply, CUE, bootstrap.** On top of the M0 engine:

- `yeet plan` — host refresh, registry digest pinning (unpinned images stated, never hidden),
  unified diff against the live release, the exact command list with runtime branches as branches,
  and the fidelity contract printed verbatim. Writes a JSON artifact.
- `yeet deploy --plan` — the artifact binds the apply: refuses on config change or host drift,
  ships the planned rendered bytes byte-for-byte.
- Embedded CUE validation (`schema.cue`): shape/enum/pattern errors as `yeet.yml:<line>: message`;
  cross-field and compose-semantic checks stay in Go.
- `yeet bootstrap` — dirs → user's `bootstrap` hook → registry login (password via stdin) →
  accessories up. Host provisioning stays the operator's; config management is a non-goal.
- Release payload staging: `env_file`s and project-relative bind mounts ship inside the release dir
  with paths rewritten — compose files reference them relative to the release, not the runner.
- Local hooks (`{run, local: true}`) + `pre_release`/`post_release`/`post_deploy` seams and
  advisory `url:` verify checks — monk's web-publish trick and smoke tests, quarantined in config.
- Rollback replays the previous release's own `yeet.snapshot.yml` choreography.
- `yeet init` — classify + scaffold + rollability doctor printing each role's exact compose delta.

**Monk cutover:** `../monk/yeet.yml` is written and validates against monk's real compose file
(8 services, 3 roles, no rollability blockers). Remaining human step: run
`SERVER_VERSION=... yeet plan && yeet deploy --plan yeet-plan.json` against production and retire
`scripts/yeet.sh`.

**M0 walking skeleton.** `yeet validate | render | deploy | rollback` work end-to-end:
config + compose loading (via `compose-go`, the loader docker compose itself uses), SSH transport
with enforced known-hosts, versioned release dirs with symlink activation and retention pruning,
and the scale–health–drain choreography with the rev 5 traffic-shift protocol (drain-before-SIGTERM
via healthcheck poisoning).

The zero-downtime claim is proven mechanically, not assumed: `YEET_E2E=1 go test ./e2e/` deploys a
Traefik+web fixture against local docker, hammers the edge with requests during a live v1→v2 roll,
and fails on a single dropped request. Latest run: **1,583 requests, 0 failures**.

Monk cutover checklist (M0 exit): write `../monk/yeet.yml` (roles `web`/`worker`/`scheduler`, job
`migrate`, accessories `traefik`/`postgres`/`redis`/`ofelia`), then
`yeet validate && yeet deploy -e production --verbose`.

Not yet built (by design, see roadmap): plan/apply + CUE (M1), journal/fencing/locks/resume/
migration gate (M2), multi-host (M3).

**Full design:** architecture, service model, deploy lifecycle, state/locking protocol, rollback
semantics, configuration, prior art, risks, and roadmap — in
[`docs/design.html`](docs/design.html) (rev 5).

The design has been through two adversarial review rounds (five independent reviewers: architecture,
market/prior-art, operations, fresh-eyes holistic, fix-verification), a generality audit, and a
rev 5 generality & robustness hardening pass (traffic-shift protocol, compose canonicalization,
multi-service roles, resource-exclusivity rule, one mutation regime). The review changelog is §13
of the design doc.

## The wedge

Two capabilities no tool in the single-server deploy niche (Kamal 2, Sidekick, Haloy, Uncloud,
Dokploy, Coolify) offers together:

1. **Plan before apply** — rendered artifacts, pinned images, and the exact remote command list,
   previewed before a single host is touched, with the plan bound to the apply.
2. **Zero-downtime for docker-compose apps as they are** — the compose file is the contract;
   scale–health–drain releases; no proprietary app model, no replacing your proxy.

## Roadmap

- **M0** — walking skeleton: deploy [monk](../monk) end-to-end, zero-downtime, from the new binary
- **M1** — plan/apply + YAML config (embedded CUE validation); retire monk's `yeet.sh`
- **M2** — trustworthy: journal, fencing, resume/abort, migration gate, per-role verify
- **M3** — second app + multi-host executor
- **M3.5** — out-of-distribution validator app (generality proven, not assumed)
- **M4** — open-source release

See §12 of the design doc for exit criteria.
