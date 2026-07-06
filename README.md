# onebox

> CLI: `ob` · domain: [onebox.run](https://onebox.run) · formerly the `yeet` codename — single-host
> is the product scope, so the product is named after the box.

**Zero-downtime deploys for docker-compose apps: agentless, plan-before-apply, keep your proxy.**

An agentless, single-binary deploy engine for apps described by a docker-compose file: preview every
deploy as a rendered diff and an exact command list, then release with health-gated zero downtime —
keeping your own proxy, database, and conventions.

## Status

**Host overview.** `ob ls` — one screen for every ob app on a host, not just the one in your
`ob.yml`. Where `ob status` is app-scoped and config-aware (recorded vs actual for the app you're
standing in), `ob ls` is a config-free inventory: it reads only what the host and Docker themselves
know — a host-wide `docker ps`, each app's `current` symlink, and the managed proxy's registry — so
it works from any directory or against `--host` with no app config at all. One line per app (recorded
release, running count, health, proxied, state: in sync / DIVERGED / NOT RUNNING / never activated),
a proxy health line, and a foreign-project footer. The whole picture is **three host reads in one
concurrent wave** (a fourth with `--incomplete`), never a per-app fan-out — it stays fast on a
high-latency link. `--json` for scripts (progress goes to stderr, so stdout stays pure JSON for
`ob ls --json | jq`); `--fail-on-drift` exits non-zero when the managed proxy is
down or any app is off its recorded release, unhealthy, or not running.

**Notifications.** `notify: { webhook: <url>, on: [failure] }` — one generic JSON POST when a
mutating verb finishes (deploy, rollback, resume, abort, accessory/proxy apply, destroy). The
journals are write-only forensics; this is the page. Payload carries app/env/host/verb/deploy
id/status/error plus a human `text` line (Slack-compatible; ntfy/Discord/generic consumers read
the fields). `on:` defaults to `[failure]` — success pings are opt-in. Fail-open: a dead
webhook is a stderr warning, never the verb's result. Error strings are the same
redaction-safe strings the terminal gets — secrets never travel, only hashes.

**M4 — managed proxy (host-scoped).** `proxy: { managed: true, config: <dir> }` closes the rev 4/5
design seam: ob owns one Traefik **per host**, shared by every ob app on the box.

- **Host scope**: `/var/lib/ob/_host/` — its own noclobber lock (TTL, `--force`), its own
  journal, and `proxy/apps/<app>` as the registration refcount. No app can claim the name.
- **Contract**: `proxy.config` is a flat dir — `traefik.yml` required (must enable `ping: {}`,
  the healthcheck gates on it, and store ACME at `/letsencrypt/acme.json`); `dynamic.yml` and
  `.env` optional. ob renders the compose around it (docker provider, `80/443`, socket ro,
  ACME bind at `_host/proxy/acme/`).
- **ACME-safe converge** (`ob proxy apply`, and `bootstrap` on first contact): unchanged
  config never touches the container; compose change → `up -d`; config-only change → upload +
  restart. Config content is never diffed or printed — `.env` may hold secrets; only hashes travel.
- **Conflict, not last-writer-wins**: every registered app must declare the same proxy config;
  a divergent hash names both apps and refuses (`--force` makes the divergence explicit).
- **Refcounted teardown**: `ob destroy` deregisters; `--proxy` removes the proxy only when no
  other app is registered — refusing (named) before any teardown otherwise.
- **Network**: role services join the external `ob-ingress` network at render time (the proxy
  project owns it); accessories and jobs stay app-private. Preflight asserts the proxy healthy
  before any deploy.
- The proxy-inside-compose shape (traefik as an accessory — monk today) remains fully supported;
  `managed: true` earns its keep only when several apps share one host and `:443`.

**CLI surface complete (design §08), single-host by design.** The gap-closure pass added the
last canonical-shape verbs:

- `ob accessory apply` — planned convergence: unified diff vs the live release, destructive-mount
  detection (named volume / absolute bind dropped → refuse without `--force`), converge under
  lock+fence+journal. Never mid-deploy.
- `ob secrets edit | push` — SOPS+age: `secrets: {sops: file}` decrypts runner-side into a
  mode-600 env file inside each release dir, injected as `env_file` on role services (closed set,
  declared). `push` hash-compares against the live release and bounces roles only on change;
  content never appears in a command or log, only hashes.
- `ob destroy` — typed app-name confirmation; volumes kept unless `--volumes` (data loss is
  opt-in); takes the lock, tears down, removes ob's state dir.
- `ob logs [role] [-f] [--tail N]` / `ob exec <role> -- <cmd>` — streamed over the transport.
- Single-host is now enforced by the schema (exactly one host per environment) and stated as the
  product boundary in the design (§02/§05); the multi-host protocol remains a sketch, deliberately
  unbuilt. `bootstrap` installs docker when absent — the one universal provisioning step.

**M3 — second app.**

- **App #2 onboarded with zero engine changes**: `ob init` against `../unlock` classified
  traefik→accessory, unlock→rolling candidate, and flagged its exact blocker
  (`container_name`); the curated `../unlock/ob.yml` validates and renders clean.
  Caveat, per the design's own anti-overfit rule: unlock is monk-shaped (traefik + one service),
  so the real distribution test remains M3.5's alien app.
- **Multi-host deferred deliberately** — the §05 protocol stays designed, the fleet executor
  unbuilt until a real fleet need exists (design §12 updated; speculative build = the
  maintenance-economics trap).
- **`ob status`** — recorded (current symlink) vs actual (each role's `ob.release` label +
  health), accessories, and any incomplete deploy. Divergence is an error exit — scriptable.
- **`ob.cue` accepted as config** — power users get let-bindings, interpolation, and
  pattern defaults; the file unifies with the same `#Config` and flows through the same pipeline.
  YAML remains the default surface.

**M2 — trustworthy.** The design's §05/§06 mechanisms, built and proven against the
ops scenarios:

- **Journal** — append-only synced JSONL per deploy at `/var/lib/ob/<app>/journal/`; every phase
  and role records intent/result; GC tied to the retention window (a journal outlives its release).
- **Lock + fencing** — noclobber lock at the authority host with TTL (host clock) + heartbeat;
  breaking a fresh lock needs `--force` and prints the holder's journal tail. Every mutating
  command is wrapped host-side against a fence file — a zombie runner dies locally (exit 97),
  no cross-host call. One regime for every mutation: deploy, resume, abort, rollback, bootstrap.
- **`ob resume`** — journal-driven: completed phases/roles skip; a half-rolled role is adopted
  via its `ob.release` label; the new epoch fences the dead runner. Proven live: runner killed
  mid-roll, resumed, **1,629 requests / 0 failures across the crash**.
- **`ob abort`** — reverts to the previous release by replaying it through the normal
  choreography (zero-downtime for rolling roles), migration-gated like auto-rollback.
- **Migration gate** — `$OB_RESULT_FILE` protocol: `changed=false` opens the gate; anything else
  fails safe. Verify failure → auto-rollback only if the gate is open or `migrations: expand-only`
  is asserted; otherwise **halt-and-page**. `--no-rollback` always halts.
- **`ob audit`** — who deployed what, when, from which SHA, incl. failed and incomplete runs.

M2 exit scenarios all pass as gated e2e tests (`OB_E2E=1 go test ./e2e/`): kill-runner→resume,
broken-worker→halt-with-old-serving, migrate-then-verify-fail→halt-and-page (unit).

**M1 — plan/apply, CUE, bootstrap.** On top of the M0 engine:

- `ob plan` — host refresh, registry digest pinning (unpinned images stated, never hidden),
  unified diff against the live release, the exact command list with runtime branches as branches,
  and the fidelity contract printed verbatim. Writes a JSON artifact.
- `ob deploy --plan` — the artifact binds the apply: refuses on config change or host drift,
  ships the planned rendered bytes byte-for-byte.
- Embedded CUE validation (`schema.cue`): shape/enum/pattern errors as `ob.yml:<line>: message`;
  cross-field and compose-semantic checks stay in Go.
- `ob bootstrap` — dirs → user's `bootstrap` hook → registry login (password via stdin) →
  accessories up. Host provisioning stays the operator's; config management is a non-goal.
- Release payload staging: `env_file`s and project-relative bind mounts ship inside the release dir
  with paths rewritten — compose files reference them relative to the release, not the runner.
- Local hooks (`{run, local: true}`) + `pre_release`/`post_release`/`post_deploy` seams and
  advisory `url:` verify checks — monk's web-publish trick and smoke tests, quarantined in config.
- Rollback replays the previous release's own `ob.snapshot.yml` choreography.
- `ob init` — classify + scaffold + rollability doctor printing each role's exact compose delta.

**Monk cutover:** `../monk/ob.yml` is written and validates against monk's real compose file
(8 services, 3 roles, no rollability blockers). Remaining human step: run
`SERVER_VERSION=... ob plan && ob deploy --plan ob-plan.json` against production and retire
`scripts/yeet.sh`.

**M0 walking skeleton.** `ob validate | render | deploy | rollback` work end-to-end:
config + compose loading (via `compose-go`, the loader docker compose itself uses), SSH transport
with enforced known-hosts, versioned release dirs with symlink activation and retention pruning,
and the scale–health–drain choreography with the rev 5 traffic-shift protocol (drain-before-SIGTERM
via healthcheck poisoning).

The zero-downtime claim is proven mechanically, not assumed: `OB_E2E=1 go test ./e2e/` deploys a
Traefik+web fixture against local docker, hammers the edge with requests during a live v1→v2 roll,
and fails on a single dropped request. Latest run: **1,583 requests, 0 failures**.

Monk cutover checklist (M0 exit): write `../monk/ob.yml` (roles `web`/`worker`/`scheduler`, job
`migrate`, accessories `traefik`/`postgres`/`redis`/`ofelia`), then
`ob validate && ob deploy -e production --verbose`.

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
