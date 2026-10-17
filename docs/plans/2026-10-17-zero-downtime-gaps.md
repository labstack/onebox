# Closing avoidable downtime in Onebox

Status: accepted, 2026-10-17. Reviewed by Codex. Implementation plan:
`docs/superpowers/plans/2026-10-17-zero-downtime-gaps.md`.

Goal: remove downtime Onebox causes without need, and make the rolling
guarantee hold when the routing layer misbehaves. Downtime inherent to one box
with single-instance services stays, and is documented.

What changed from v1:

- Proxy: an explicit state machine, an invalidate-first marker, fresh-output
  evidence, and no Traefik restart on a discovery repair.
- Services: a `--timeout` on apply and a graceful destroy, beside the rendered
  grace period.
- Rolling: the gate runs before every retirement and before success (covers
  resume), matches the expected proxy services, and fails rather than warns.
- New: the deploy plan names the proxy action.
- New: recovery restores the predecessor before removing serving containers.
- The rejection of dynamic hot reload stays, with a corrected rationale.

---

## 1. Proxy: restart Traefik only when Traefik's inputs change

### Problem

The applied-state digest covers config files, dynamic files and the rendered
`compose.yaml` (`internal/proxy/proxy.go:484`, `:522`). The compose file pins
discovery to the runner release (`internal/proxy/proxy.go:52`; development
builds use `:edge`). A new release therefore changes the digest, and
`EnsureProxy` swaps both directories, force-recreates discovery, and restarts
an unchanged Traefik container (`internal/engine/proxy.go:211`).

Two existing defects share the code path:

- A missing discovery container or output with an unchanged digest takes the
  same path, so repairing discovery restarts a healthy Traefik.
- The marker is written after health but never invalidated before the swap.
  Interrupted apply of B, then a re-apply of A: the marker still reads A, so
  the run is a no-op against B's files on disk.

### Design

Staging returns a pair `{Config, Compose}`. One type owns its encoding
(`<config> <compose>` on one line) and its parsing. Anything unparsable,
including the legacy one-digest marker, is "unknown".

`EnsureProxy` decides from the staged pair, the marker, and observation:

- **No-op.** Marker equals staged, Traefik and discovery running, output
  present.
- **Config path.** `marker.Config` differs or is unknown:
  1. Delete the marker.
  2. Stage, swap directories and `compose.yaml` (as today).
  3. Force-recreate discovery. Wait for `onebox.yml`. The swap leaves no output
     file, so its presence proves the new controller published.
  4. `up -d proxy`. Restart Traefik if Compose did not recreate it.
  5. Wait for health. Write the marker atomically (temporary file, `mv`).
- **Compose or repair path.** Config equal; Compose differs, or a container or
  the output is missing:
  1. Delete the marker.
  2. Replace `compose.yaml` atomically. No directory swap.
  3. Record the discovery container id and the output's inode. Bring
     discovery up: `--force-recreate` when it is not running or the output is
     missing (a running controller that lost its output will not rewrite it
     by itself), plain `up -d` otherwise, which recreates it only on drift.
     Then `up -d proxy`, which recreates Traefik only if its own block drifted.
  4. If the discovery id changed or the output was missing, wait until the
     output appears or its inode changes. Discovery writes through
     temporary file and rename on every reconcile, including startup
     (`cmd/onebox-discovery/main.go:42`, `internal/discovery/docker.go:170`),
     so a new inode proves the new controller published.
  5. Never `docker restart`. If Compose recreated Traefik, wait for health.
  6. Write the marker atomically.

The restart rule is now one line: restart only on the config path.

`ob status` parses the pair and says which half drifted, instead of printing
the first 8 characters of one string (`internal/engine/proxystatus.go:294`,
`internal/engine/status_snapshot.go:453`).

A legacy marker takes the config path once: one last restart per host.

### Assumption to verify

The compose path relies on Compose not recreating `proxy` when its dependency
`discovery` is recreated (`depends_on` with `condition: service_started`, no
`restart: true`), and on `depends_on` being outside the service hash. That
holds in the Compose v2.39.4 source. Onebox sets no Compose version floor
(`internal/app/prerequisites.go:120`). Verify on the target hosts' Compose
version before release; an end-to-end test should assert the Traefik
container id survives a discovery image change.

### Why not a separate Compose project for discovery

It needs a migration of the live `onebox-discovery` container, plus status,
destroy, preflight and naming-guard changes, and it solves none of the state
transitions above.

---

## 2. Managed services: a clean shutdown window

### Problem

Rendered service documents set no `stop_grace_period`
(`internal/app/services.go:367`), so Docker kills after 10s. PostgreSQL fast
shutdown writes a checkpoint; durable Redis and Valkey write on shutdown.
Either can exceed 10s, and a kill makes the restart slower. Destroy uses
`docker rm -f` (`internal/engine/services.go:482`) and skips shutdown entirely.

### Design

One constant, `serviceStopGrace = 2m`, used in three places:

- Rendered `stop_grace_period`. Covers reboots and manual stops.
- `docker compose ... up -d --timeout 120` in service apply
  (`internal/engine/services.go:141`). Protects the first recreate, because
  existing containers still carry the 10s default.
- Destroy: `docker stop -t 120`, then `docker rm -f`. The label sweep stays,
  so it works without the Compose file.

This is a bounded policy, not a promise that every database finishes in time.
Apply handles services one at a time, so the worst case grows per service. The
90s health budget starts after Compose returns and does not change.

Rollout: every managed service is recreated once on its next apply, and
PostgreSQL services with declared extensions on their next deploy. Release
note, as with #220. Contract goldens change
(`internal/app/equivalence_test.go:111`).

---

## 3. Rolling: gate on published routing, not only a fixed sleep

### Problem

After a newcomer is healthy, `retireContainer` sleeps a fixed 3s and retires an
old replica (`internal/engine/roll.go:263`). Nothing checks that discovery
published the newcomer. Resume adopts running newcomers without any check
(`internal/engine/roll.go:145`, `:163`). If discovery is down, `onebox.yml`
stays frozen: it keeps old addresses and never gains the newcomer, so the
route ends on dead endpoints while the deploy reports success.

### Design

Only when the proxy is managed and the workload has routes.

`published(container)` reads and parses `onebox.yml`, takes the container's
ingress address with discovery's own selection rule (shared helper, from
`internal/discovery/docker.go:102`), and checks it is a server of every
expected service, `ProxyServiceFor(workload, i)` (`internal/app/generate.go:543`).

- **Gate A**, before any retirement and before reporting success: every
  newcomer is healthy and published. Covers fresh surges and resumed
  adoption. Bound 30s. On failure, a newcomer created in this pass is removed,
  adopted ones are left, and old replicas keep serving.
- **Gate B**, after the drain flip and before any signal or stop: the drained
  address is absent from the expected services. On failure, stop the roll
  with the container still running and draining. `ob resume` retries.
- An unreadable, unparsable or cancelled read never counts as presence or
  absence.
- Unguardable health checks (no shell in the image,
  `internal/engine/roll.go:275`) cannot drain, so Gate B does not apply. The
  existing log line stays.
- `ConvergeBuffer` stays after each gate. Published is not loaded: Traefik's
  file watch and throttle (2s default, changeable by custom static config) are
  not observable without the Traefik API. Documented as an assumption.

Residual risk, accepted: a frozen document plus Docker reusing a just-freed
address could pass Gate A falsely. Docker allocates addresses serially, so
immediate reuse is unlikely.

---

## 4. The deploy plan names the proxy action

Today the plan diffs only the application Compose
(`internal/onebox/plan_deploy.go:128`), and the operation graph has no proxy
step (`internal/onebox/operation_graph.go:36`). A deploy can restart Traefik
without the plan saying so.

Add one plan line, computed with the staging `ob status` already does:
`proxy: unchanged`, `proxy: update controller (no restart)`, or
`proxy: restart (configuration changed)`.

---

## 5. Recovery: restore before removing serving containers

Recovery force-removes every container of the interrupted release
(`internal/engine/recovery.go:169`) before restoring the predecessor
(`:171`). If the interrupted release was serving, `ob abort` has a gap.

All releases share one Compose project (`internal/engine/roll.go:25`), so a
roll of the predecessor already treats the interrupted release's containers of
that workload as old replicas and drains and retires them.

New order:

1. Remove the interrupted release's containers that are not serving a
   workload the predecessor will roll: anything not running, job and hook
   containers, containers of `recreate` workloads, and any container holding
   the transient rollout name (`TransientContainer`), which the predecessor's
   roll needs. With no predecessor, remove everything (today's behavior).
2. Restore the predecessor (unchanged). Rolling workloads drain and retire the
   interrupted containers that were kept.
3. Sweep every remaining interrupted-release container (today's removal).
4. Verify, then the existing postcondition.

Lands last, after the rolling gate, because the restore relies on adopted
newcomers being admitted correctly.

---

## Rejected: hot reload of authored dynamic files

Per-file updates in `dynamic/` can expose a half-updated set to Traefik's
directory rebuild. Today's swap is not transactional either, and dynamic
changes do not inherently need a restart: a single generated document,
replaced atomically inside a stable directory as discovery already does, would
work. That is a separate feature. This patch keeps the restart.

---

## Downtime that remains, by design

- Any changed managed-service configuration restarts that service's single
  container.
- Any change to Traefik's static, dynamic or authored configuration, image or
  ports restarts or recreates Traefik.
- `recreate` workloads keep their stated gap.
- Host events: Docker daemon restart, reboot, host failure.

## Tests

- Proxy: compose-only change (no swap, no restart, marker updated); config
  change (today's sequence); legacy marker; discovery repair without restart;
  stale output rejected; interruption between each swap step, then a re-apply
  of the old input; status shows which half drifted. Keep the existing
  missing-controller and missing-output tests (`internal/engine/proxy_test.go:203`,
  `:286`) and the marker-after-health assertion.
- Services: rendered grace period; `--timeout` on apply; graceful destroy;
  goldens regenerated.
- Rolling: unpublished newcomer fails and old replicas keep serving; adopted
  unhealthy or unpublished newcomer blocks retirement; withdrawal timeout
  stops before signal; unreadable document is neither presence nor absence;
  unguardable health keeps today's path. Keep the resume adoption test
  (`internal/engine/roll_test.go:174`).
- Plan: each of the three proxy lines.
- Recovery: a serving interrupted release is drained by the predecessor's
  roll, not force-removed first; no predecessor keeps today's removal.
