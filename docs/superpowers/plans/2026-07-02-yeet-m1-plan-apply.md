# yeet M1 — plan/apply, CUE, bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Everything monk's 420-line `yeet.sh` does that belongs in a deploy engine, per design §12 M1: embedded CUE validation, `yeet plan` (refresh → diff → pinned images → command list → artifact) bound to `deploy --plan`, `yeet bootstrap`, plus the gaps a real monk cutover exposed.

**Architecture:** Builds on the M0 engine. CUE validates shape before Go decodes (division of labor: CUE = shape/enums/patterns via a closed `#Config`; Go keeps cross-field + compose-semantic checks). Plan artifact stores the *verbatim rendered compose bytes* — apply ships exactly what was planned, with drift measured over {config hash, current release id, running image ids} (design §02 drift set). Payload staging closes the env_file/relative-bind gap. Hooks gain a `local:` form (runner-side) so monk's web-publish trick stays quarantined in monk's config, not in the engine.

**Tech Stack additions:** `cuelang.org/go` (pinned, thin wrapper per design §11), `github.com/pmezard/go-difflib` (unified diff for plan output).

## Global Constraints

- All M0 constraints hold (closed injection set, --no-deps, command-injection rules, transport shell contract).
- CUE never reaches the user: errors are reworded to `yeet.yml:<line>: <plain message>` (design §02).
- Plan fidelity contract (design §01) is printed in `plan` output verbatim: config > images > choreography > hooks; hooks flagged unplannable; runtime branches shown as branches.
- The plan artifact binds the apply: `deploy --plan` refuses on config-hash mismatch or host drift, and deploys the artifact's rendered bytes verbatim. Payload files (env/bind sources) are re-staged at apply — a stated fidelity limit, not silent.
- Bootstrap provisions only what is generic (dirs, registry auth, accessories); host-specific provisioning (docker install, tailscale, NFS) is the user's `bootstrap` hook — config management stays a non-goal (design §02).
- No journal/fencing/locks — that's M2. Do not implement ahead.

## Tasks

### Task 1: CUE embedded validation
- Files: `internal/config/schema.cue`, `internal/config/cue.go`, `internal/config/cue_test.go`; modify `config.Load` to run CUE first.
- `//go:embed schema.cue`; `#Config` closed definition: app pattern, compose, environments (min 1 host), roles (mode enum, ready/drain shapes, duration pattern `^[0-9]+(ms|s|m|h)$`), accessories/jobs ident lists, hooks `string | {run: string, local?: bool}`, verify entries `{http|exec|url, role?, port?, contains?, advisory?}`, registry `{server, username, password_env}`, proxy, retain int.
- `ValidateCUE(yamlBytes, filename) error` — `cuecontext` + `encoding/yaml.Extract` (keeps line positions), unify with `#Config`, `Validate(cue.Concrete(true))`; reword via `cuelang.org/go/cue/errors` positions.
- Tests: valid sample passes; typo'd field (`rolez:`) errors with line number; bad mode enum errors mentioning allowed values; hook map form accepted.

### Task 2: Release payload staging (monk blocker)
- Files: `internal/compose/payload.go`, `internal/compose/payload_test.go`; modify `render.go`/`release.Stage` call path.
- After Render: for every service, collect bind-mount sources that live **inside the project dir** (compose-go absolutized them): copy into staging under their project-relative path and rewrite the rendered YAML source to `./<rel>`. Sources outside the project dir (`/var/run/docker.sock`, `/data/...`) stay absolute, untouched. Verify (test) whether compose-go folds `env_file` into `environment` at load; if entries survive, stage + rewrite them the same way.
- `StagePayload(p *types.Project, projectDir, stagingDir string) (rewrites map[string]string, err)` + `RewriteSources(rendered []byte, rewrites) []byte` (string-replace of absolute paths in the marshaled YAML — paths are unique absolutes).
- Tests: fixture with `env_file: .env` + `./conf/app.conf:...:ro` + `/var/run/docker.sock` — staging contains `.env`/`conf/app.conf`, rendered YAML has `./conf/app.conf`, docker.sock untouched.

### Task 3: Hook forms + advisory URL verify (monk cutover)
- Files: modify `internal/config/config.go` (Hook type w/ custom UnmarshalYAML: string → host, map → {run, local}), `internal/engine/recreate.go` RunHook (local hooks exec runner-side via `sh -c`, cwd = config dir, env `YEET_APP/YEET_HOST/YEET_RELEASE_DIR/YEET_RELEASE_ID`), `internal/engine/verify.go` (VerifyCheck.URL + Contains + Advisory: runner-side HTTP GET; advisory failures warn, never fail), `internal/engine/deploy.go` (seams: `pre_release` hook before roles, `post_release` after roles/before verify, `post_deploy` after finalize — all already-named design seams).
- Tests: local hook runs on runner (Fake transport records nothing); advisory URL failure does not fail Verify; authoritative checks unchanged; seam ordering asserted in Deploy phase test.

### Task 4: Transport.RunInput + bootstrap
- Files: `internal/transport/*.go` (add `RunInput(ctx, cmd, stdin string)` to interface + Local/SSH/Fake), `internal/config` Registry field, `internal/engine/bootstrap.go`, `cmd/yeet/commands.go` (`yeet bootstrap`).
- Bootstrap order: mkdir base/releases → `bootstrap` hook (host, verbatim — monk puts docker/tailscale/NFS here) → registry login if configured (`docker login <server> -u <user> --password-stdin`, password from `password_env`, via RunInput — never in the command string) → stage+push a `-bootstrap` release dir → `up -d --no-recreate <accessories>` from it. No activation.
- Tests: command sequence + stdin used for login (Fake records `[stdin]` marker); missing password_env errors before touching host.

### Task 5: `yeet plan` + `deploy --plan` binding (the roadmap centerpiece)
- Files: `internal/plan/plan.go`, `internal/plan/plan_test.go`, `internal/engine/describe.go`, `cmd/yeet/commands.go`.
- `Refresh(ctx, t, cfg)` → `HostState{CurrentRelease string, ImageIDs map[service]string}` (running container image ids via docker inspect — the drift set).
- Digest pinning: host-side `docker buildx imagetools inspect <ref> --format '{{.Manifest.Digest}}'` per role/job image → rewrite `image: name@sha256:...` before render; on failure warn `unpinned (buildx unavailable)` — stated, not hidden.
- Diff: `cat <current>/compose.yaml` (if any) vs rendered → unified diff (difflib).
- `Describe(cfg)` — the command list with runtime branches as branches (mirrors RollRole/RecreateRole constants; placeholders `<old>/<new>`).
- Artifact JSON: `{ID, App, Env, CreatedAt, GitSHA, ConfigHash, HostState, PinnedImages, RenderedCompose, Commands}`; `yeet plan [-o yeet-plan.json]` prints fidelity contract + diff + images + commands, writes artifact.
- `yeet deploy --plan f`: env must match; sha256(yeet.yml) == ConfigHash; fresh Refresh equals artifact.HostState (else "drift — re-plan"); stage artifact.RenderedCompose verbatim + re-staged payload; deploy with the artifact's release ID.
- Tests: artifact roundtrip; binding refuses on config change; refuses on drift (different running image id); applies planned bytes (staging compose.yaml == artifact.RenderedCompose).

### Task 6: Rollback snapshot replay (closes M0 honesty note)
- Files: `internal/config/config.go` (`LoadBytes`), `internal/engine/deploy.go` Rollback.
- Rollback: `cat releases/<prev>/yeet.snapshot.yml` → LoadBytes + Validate → use the SNAPSHOT's Roles/Order/Verify for choreography (old release, old config, old modes — design §06). Fallback with loud warning if snapshot missing/unparseable (pre-M1 releases).
- Tests: snapshot with different order/mode drives the replay; missing snapshot falls back with warning.

### Task 7: `yeet init` doctor
- Files: `cmd/yeet/init.go`, `cmd/yeet/init_test.go`.
- Heuristics: well-known images (postgres|mysql|mariadb|redis|valkey|memcached|rabbitmq|kafka|minio|traefik|ofelia|dozzle) → accessory; service named/commanded `migrate` → job; rest → roles (rolling if it has a healthcheck or traefik labels, else recreate). Emit yeet.yml; print the rollability delta per would-be-rolled role (drop container_name / unbind host port) — the rev 5 "doctor, not a wizard".
- Tests: monk-shaped fixture classifies correctly; doctor lines name the exact violations; refuses to overwrite existing yeet.yml.

### Task 8: Monk cutover artifact + docs + full suite
- Write `../monk/yeet.yml` (roles web/worker/scheduler, jobs migrate, accessories traefik/postgres/redis/ofelia, hooks: bootstrap(host)/migrate(host)/pre_release+post_release+post_deploy(local web publish), verify healthz + advisory edge URLs) — validated with `yeet validate -c ../monk/yeet.yml` (no deploy).
- README: M1 status; e2e rerun; gofmt/vet; changelog note in design doc §12 not needed (roadmap unchanged).

## Self-Review
Covered: CUE (roadmap ✔), plan+binding+fidelity (✔), bootstrap (✔), render existed (M0 ✔). Monk-cutover extras justified by the yeet.sh audit: payload staging, local hooks, advisory URL verify, snapshot replay, init. Deliberately absent: journal/locks/resume (M2), managed proxy (M2+, monk's traefik is an accessory), docker/tailscale install (bootstrap hook, non-goal).
