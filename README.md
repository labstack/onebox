# yeet

> Working codename — the release gets a searchable, non-slang name.

**Zero-downtime deploys for docker-compose apps: agentless, plan-before-apply, keep your proxy.**

An agentless, single-binary deploy engine for apps described by a docker-compose file: preview every
deploy as a rendered diff and an exact command list, then release with health-gated zero downtime —
keeping your own proxy, database, and conventions.

## Status

**M3 implemented — second app, single-host by decision.**

- **App #2 onboarded with zero engine changes**: `yeet init` against `../unlock` classified
  traefik→accessory, unlock→rolling candidate, and flagged its exact blocker
  (`container_name`); the curated `../unlock/yeet.yml` validates and renders clean.
  Caveat, per the design's own anti-overfit rule: unlock is monk-shaped (traefik + one service),
  so the real distribution test remains M3.5's alien app.
- **Multi-host deferred deliberately** — the §05 protocol stays designed, the fleet executor
  unbuilt until a real fleet need exists (design §12 updated; speculative build = the
  maintenance-economics trap).
- **`yeet status`** — recorded (current symlink) vs actual (each role's `yeet.release` label +
  health), accessories, and any incomplete deploy. Divergence is an error exit — scriptable.
- **`yeet.cue` accepted as config** — power users get let-bindings, interpolation, and
  pattern defaults; the file unifies with the same `#Config` and flows through the same pipeline.
  YAML remains the default surface.

**M2 — trustworthy.** The design's §05/§06 mechanisms, built and proven against the
ops scenarios:

- **Journal** — append-only synced JSONL per deploy at `/var/lib/yeet/<app>/journal/`; every phase
  and role records intent/result; GC tied to the retention window (a journal outlives its release).
- **Lock + fencing** — noclobber lock at the authority host with TTL (host clock) + heartbeat;
  breaking a fresh lock needs `--force` and prints the holder's journal tail. Every mutating
  command is wrapped host-side against a fence file — a zombie runner dies locally (exit 97),
  no cross-host call. One regime for every mutation: deploy, resume, abort, rollback, bootstrap.
- **`yeet resume`** — journal-driven: completed phases/roles skip; a half-rolled role is adopted
  via its `yeet.release` label; the new epoch fences the dead runner. Proven live: runner killed
  mid-roll, resumed, **1,629 requests / 0 failures across the crash**.
- **`yeet abort`** — reverts to the previous release by replaying it through the normal
  choreography (zero-downtime for rolling roles), migration-gated like auto-rollback.
- **Migration gate** — `$YEET_RESULT_FILE` protocol: `changed=false` opens the gate; anything else
  fails safe. Verify failure → auto-rollback only if the gate is open or `migrations: expand-only`
  is asserted; otherwise **halt-and-page**. `--no-rollback` always halts.
- **`yeet audit`** — who deployed what, when, from which SHA, incl. failed and incomplete runs.

M2 exit scenarios all pass as gated e2e tests (`YEET_E2E=1 go test ./e2e/`): kill-runner→resume,
broken-worker→halt-with-old-serving, migrate-then-verify-fail→halt-and-page (unit).

**M1 — plan/apply, CUE, bootstrap.** On top of the M0 engine:

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
- **M3** — second app (multi-host: out of scope by design — no fleet need)
- **M3.5** — out-of-distribution validator app (generality proven, not assumed)
- **M4** — open-source release

See §12 of the design doc for exit criteria.
