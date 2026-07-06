# ob ls — host-wide overview Implementation Plan

**Goal:** A single read-only command that inventories **every app on the host** — recorded release, running/health, drift, and whether it's behind the managed proxy — plus a one-line health summary of the shared proxy itself. `ob status` answers "is *this* app in sync?" (detailed, per-app, config-aware); `ob ls` answers "what's on this box and is anything wrong?" (coarse, host-wide, config-free). The multi-app capability already exists in the on-disk layout (`/var/lib/ob/<app>/`) and the proxy registry (`/var/lib/ob/_host/proxy/apps/`); this command surfaces it. Exit = `ob ls` (from any project dir, or `ob ls --host user@host`) prints a proxy line plus one row per app and flags NOT RUNNING / DRIFTED / unhealthy at a glance.

**Architecture:** Mechanisms, not claims (design §02). `ob ls` is round-trip-bound like `ob status`, so it reuses that command's model: fire every read concurrently in one `gather()` wave behind a `ui.Busy` spinner, then render a pure table. Three host reads, no per-app fan-out:

1. **One host-wide `docker ps`** — NO project filter — with `--format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}|{{.Label "ob.release"}}|{{.Status}}'`. Grouped by the compose-project label, this is the running state + health (via `healthFromStatus` on `.Status`) + actual release of **every** container on the host — all apps AND the `ob-proxy` project — in a single call.
2. **One recorded-release enumeration.** The base root is `OB_BASE_DIR` or `/var/lib/ob`, resolved **in Go** (never hardcoded — the e2e/macOS hook overrides it) via a new `release.Root()`. The command lists that dir and reads each app's `current`, POSIX-safe (no bash `nullglob`; mirrors `journal.Journals`):
   `cd <root> 2>/dev/null && for a in $(ls -1 2>/dev/null); do [ "$a" = _host ] && continue; [ -d "$a" ] || continue; printf '%s|%s\n' "$a" "$(readlink "$a/current" 2>/dev/null)"; done || true`
   This is the source of truth for "which apps exist" (an app is a `<root>/<app>` dir) and their recorded release; it surfaces apps that exist but aren't running, and is empty-host-safe.
3. **One proxy-registry read** — `ls -1 <root>/_host/proxy/apps` — the set of apps registered behind the managed proxy (`registerProxyApp` writes these), for the PROXY column.

Rows and the proxy summary are derived purely from (1)+(2)+(3): recorded from (2); each app's containers from (1) keyed by project; proxied from (3); the proxy summary from (1)'s `ob-proxy` project (running + health only — NOT config drift or cert runway, which would cost the extra `_host` reads that are `status`'s job). `ob ls` never reads a per-app `ob.yml`, so it deliberately cannot report role modes, expected accessories, or config drift — that's `status`'s depth. It reports only what the host and Docker themselves know. Because it makes an *assertion about nothing* (it's an inventory), `ob ls` exits 0 even when rows show problems — unlike `status`, which exits non-zero on divergence. (`--fail-on-drift` is offered for CI.)

Connection (two entry points, decided for v1):
- **Ambient `ob.yml`** — a read-only verb via `loadAllLenient`; uses that config's `--env` host only to open the SSH connection, then enumerates the whole host regardless of which app's dir you're in.
- **`--host user@host`** — config-free: dial directly via `transport.NewSSH`, no `ob.yml` required. A host-level command must not require an app's config to exist; this is the honest entry point and is small because `HostList` takes a plain `transport.Transport` (below), not an `Engine`.

`HostList` is a **package function**, `HostList(ctx, t transport.Transport, u *ui.UI, opts ListOptions) ([]AppRow, ProxySummary, error)` — NOT an `Engine` method. It needs only the transport, the UI (for `Busy`), and the host root; keeping it off `Engine` means neither entry point has to fabricate an app `Engine`/config, and it unit-tests against `transport.Fake` directly.

## Global Constraints

- **No new round-trip regressions.** The whole command is ≤ 3 concurrent host reads regardless of app count (the proxy line reuses read 1; `--incomplete`, below, adds exactly ONE more, never O(apps)). No per-app fan-out, no per-container inspect (health comes from `docker ps .Status`). Reuse `gather`, `healthFromStatus`, `parseContainerLine`, and `ui.Busy`.
- **Never hardcode the base path.** All host paths derive from `release.Root()` / `proxy.HostPaths()`, both of which honor `OB_BASE_DIR`. A hardcoded `/var/lib/ob` silently breaks every e2e run.
- **POSIX shell only.** The remote is `sh`, not bash — no `nullglob`, no arrays. Enumeration uses `ls` + per-entry guards (`[ -d "$a" ]`), so an empty host yields no rows rather than a literal-glob "app named `*`".
- **Injection rule (design §11).** `ob ls` interpolates **nothing** it reads back into a later command (it only reads + renders), so command-injection risk is nil — but app names parsed from the filesystem must pass `appNameRe` before display/JSON to reject a hostile directory name, and container ids from read 1 still pass `validID` in `parseContainerLine`.
- **Secrets never printed (design §07).** `ob ls` reads no `.env`, no `acme.json`, no config bytes — only release ids, container labels, health words, and app names.
- **`_host` is not an app; `ob-proxy` is not a tenant.** Exclude `<root>/_host` from enumeration; the `ob-proxy` project from read 1 feeds the proxy summary line, not an app row. Foreign compose projects (a read-1 project with no `<root>/<project>` dir and not `ob-proxy`) are counted in a one-line footer, not shown as apps.
- **Read-only, no mutation, no lock.** `ob ls` acquires no lock and mutates nothing; safe to run any time, including mid-deploy of some app (a half-rolled app simply shows DRIFTED / mixed releases, which is correct).

## Tasks

### Task 1: root helper + enumeration reads (`internal/release/release.go`, `internal/engine/hostlist.go`)
- Add `release.Root() string` — `OB_BASE_DIR` or `/var/lib/ob` (factor it out of `PathsFor`, which becomes `Root()+"/"+app`).
- Factor the container-line parse out of `projectContainers` into `parseContainerLine(line string) (id, svc, release, health string, ok bool)` (validates `id` with `validID`, health via `healthFromStatus`); `projectContainers` and the new host read share it.
- `hostContainers(ctx, t) (map[string][]svcContainer, error)` — read 1 (host-wide `docker ps`), keyed by **project** label; keep the `ob-proxy` project (needed for the summary), drop empty-project lines.
- `recordedReleases(ctx, t) (map[string]string, error)` — read 2 (the `ls`+`readlink` command above), app→recorded (basename of the symlink target; `""` when never activated). `appNameRe`-validate keys.
- `proxyRegisteredApps(ctx, t) (map[string]bool, error)` — read 3.
- All four unit-tested against `transport.Fake`.

### Task 2: `HostList` derivation (`internal/engine/hostlist.go`)
- `type appState int` ∈ {inSync, notRunning, neverActivated, diverged, running unrecorded}; `type AppRow struct { App, Recorded string; Running int; Health string; Proxied bool; State appState }`; `type ProxySummary struct { Running bool; Health string }`; `type ListOptions struct { Incomplete bool }`.
- `HostList(ctx, t, u, opts)` runs reads 1–3 (and, iff `opts.Incomplete`, ONE host-wide journal scan — a single command that finds any app with an unfinished deploy, NOT a per-app loop) in one `gather(...)` wave behind `ui.Busy("querying " + t.Host())`, then derives:
  - **app set = read-2 keys** (every running non-foreign project necessarily has a dir, so it's already a key; a read-1 project not in the set and not `ob-proxy` is *foreign* → footer count).
  - per app: recorded from read 2; containers from read 1; `Running` = len; `Health` summary with precedence **unhealthy > starting > healthy > none** ("2 unhealthy" / "1 starting" / "healthy" / "-"); proxied from read 3.
  - **state:** recorded=="" ∧ 0 containers → `neverActivated`; recorded=="" ∧ containers>0 → `runningUnrecorded`; recorded set ∧ 0 containers → `notRunning`; any container `ob.release != recorded` OR any unhealthy → `diverged`; else `inSync`. (STATE alone collapses drift and unhealth into `diverged`; the HEALTH column recovers the distinction — documented, acceptable for an overview.)
  - proxy summary from read 1's `ob-proxy` containers (running + `Health`).
  - rows: table order = problems-first then alphabetical (stable); JSON order = alphabetical only (stable for diffing/scripting).

### Task 3: the `ls` command + rendering (`cmd/ob/commands.go`, `internal/engine/hostlist.go`)
- `root.AddCommand` an `ls` cobra command. Resolve the transport from **either** the ambient `ob.yml` (`loadAllLenient` → the env host) **or** `--host user@host` (`transport.NewSSH` directly); error clearly if neither is available.
- Render: a proxy header line (`proxy  ob-proxy  <health>` or `NOT RUNNING ⚠`), then `APP  RECORDED  RUNNING  HEALTH  PROXY  STATE`, styled via `ui.OK/Warn/Dim`. Empty host → "no apps deployed on <host>". Footer: "N foreign compose project(s) not managed by ob" when any exist.
- Flags: `--host` (string), `--json` (emit `{proxy, apps, foreign}` with stable alphabetical `apps`), `--fail-on-drift` (exit non-zero if any row is `notRunning`/`diverged`/`runningUnrecorded`; `neverActivated` does NOT fail — a staged-but-unlaunched app isn't a regression), `--incomplete` (the one extra host-wide journal read). Default exit is 0.

### Task 4: tests (`internal/engine/hostlist_test.go`, `cmd/ob`)
- `Fake`-driven `HostList` with `OB_BASE_DIR` set: read 1 spans projects `monk`, `blog`, `ob-proxy`, and a foreign `grafana`; read 2 returns `monk`, `blog`, `api` (dir, not running), `stale` (dir, never activated); read 3 = `monk blog`. Assert: `ob-proxy`→proxy summary (not a row); `grafana`→foreign footer (not a row); `monk` inSync, `blog` diverged (older `ob.release`), `api` notRunning, `stale` neverActivated; an unhealthy container → its app diverged with the right HEALTH; PROXY column from the registry; problems-first table order vs alphabetical JSON.
- **Round-trip budget:** exactly 3 host reads default (one `docker ps`, one enumeration, one proxy `ls`); exactly 4 with `--incomplete`.
- Empty-host (`ls` returns nothing) → "no apps"; `--fail-on-drift` exit codes; `--host` path builds a transport without an `ob.yml`; `--json` shape.

### Task 5: docs + follow-ups
- README: add `ob ls` to the verb list; one paragraph on the app-vs-host split (`status` = one app, config-aware, exits on drift; `ls` = whole host, config-free, inventory).
- Note the deferred companion (not in this plan): `ob doctor` — host-readiness (sshd `UseDNS`/`GSSAPI` handshake tax, docker/compose versions, disk headroom, proxy cert runway).

## Self-Review

- **Why filesystem scan as source-of-truth, not the proxy registry?** The registry only knows *proxied* apps; an app with `proxy: kind: none` would be invisible. `<root>/<app>` exists for every ob-deployed app; read 2 is authoritative and read 3 is only the PROXY flag.
- **Why exit 0 on problems?** `ob ls` is an inventory, not an assertion about one app; a diverged app is information, not this command's failure. CI gates with `--fail-on-drift`. `status` still exits non-zero — contract unchanged.
- **Why is `HostList` a function, not an `Engine` method?** So neither entry point (ambient `ob.yml` or `--host`) has to fabricate an app `Engine`/config for a host-level query. It needs only `transport.Transport` + `ui.UI` + `release.Root()`.
- **Round-trip claim.** Default = 3 reads in one `gather` wave (~one wave + the SSH handshake, same envelope as optimized `ob status`); `--incomplete` = 4 (one host-wide journal scan, never per-app).
- **`OB_BASE_DIR` / POSIX-shell traps.** Both were bugs in the first draft: a hardcoded `/var/lib/ob` breaks e2e, and a `for d in <base>/*/` loop invents an app named `*` on an empty host. Fixed via `release.Root()` and an `ls`+guard enumeration mirroring `journal.Journals`.
- **Proxy visibility.** The shared ingress is the one host-global component whose failure takes down every app, so it gets a summary line (from read 1, no extra round trip) rather than being excluded outright — a gap in the first draft.
- **Injection.** Nothing read here is interpolated into a later command; app names are still `appNameRe`-validated and ids `validID`-validated before display so a hostile directory or label can't smuggle control sequences into the table/JSON.
- **Foreign containers.** Counted, not hidden (silent omission would misrepresent the host), but not promoted to app rows since ob can't manage them.
