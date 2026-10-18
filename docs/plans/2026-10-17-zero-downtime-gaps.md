# Closing avoidable downtime in Onebox

Status: implemented, 2026-10-17. Reviewed by Codex. Implementation plan:
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
2. Restore the predecessor. A workload counts as already restored only when
   its containers match the predecessor's revision and none of them belongs
   to the interrupted release; matching revision alone is not enough, because
   step 3 would then remove the only containers it has. Otherwise the
   predecessor rolls it, which drains and retires the interrupted containers
   that were kept.
3. Sweep every remaining interrupted-release container (today's removal).
4. Verify, then the existing postcondition.

Lands last, after the rolling gate, because the restore relies on adopted
newcomers being admitted correctly.

---

## 6. Review round decisions (2026-10-17)

A whole-branch review of the first implementation found these gaps. Each
decision below amends the section it names.

- **R1, withdrawal (amends §3 Gate B).** Withdrawal is checked whenever the
  proxy is managed, whatever the new role's routes are. A drained container
  with no address on the proxy network is withdrawn: it cannot be a server.
  Otherwise it is withdrawn when its address is a server of no service in
  the document, not only of the new role's expected services. Adding a
  route, removing one, or changing its protocol then retires old replicas
  normally. Gate A keeps checking the new role's expected services.
- **R2, converge marker (amends §1).** The converge path never touches
  `config/` or `dynamic/`, so it keeps the Config half: it writes
  `<applied config> sha256:<64 zeros>` atomically instead of deleting the
  marker. A failed or interrupted converge is retried as a converge, not as
  a configuration apply with a Traefik restart. The config path still
  deletes the marker.
- **R3, fresh publication on the converge path (amends §1).** Running
  discovery rewrites `onebox.yml` on every container event, so an inode
  change alone does not prove a new controller published. The converge path
  stops discovery, samples the inode, brings discovery up (forced when it
  was missing or the output was missing), then waits for a new inode.
  Traefik keeps serving the last document meanwhile.
- **R4, Traefik replacement is named (amends §1, §4).** The observation
  compares the live and rendered `services.proxy` blocks (parsed YAML; an
  unreadable live file counts as changed). A compose drift that changes
  Traefik's own block is the action `recreate Traefik (its container
  definition changed)`. It runs exactly like the converge path. The plan says
  `start Traefik` when Traefik is not running.
- **R5, the config path keeps serving until the restart (amends §1).** Live
  `config/` and `dynamic/` are renamed into the staged directory instead of
  deleted. Traefik's bind mounts follow the renamed directories, so it keeps
  its routes until it is restarted or recreated. The retired directories go
  with the staged directory after health; on failure they stay for the next
  run, which restarts Traefik.
- **R6.** The converge path uploads only `compose.yaml`, never `config/`
  (which can hold `.env` credentials).
- **R7, single-replica recovery (amends §5).** A running interrupted
  container holding the transient rollout name is renamed to the lowest free
  slot name instead of removed, so it keeps serving and frees the name the
  predecessor's roll needs. Before restoring, the drain file is removed from
  the predecessor's running replicas, so a replica drained by the
  interrupted roll becomes healthy again and can be admitted.
- **R8, a failed replace stays a replace (amends R2, R4).** The replace path
  writes its own pending digest, `sha256:<63 zeros>1`, and a marker carrying
  it counts as a Traefik definition change. A retry after the live
  `compose.yaml` was already moved is planned and labelled as a replace.
- **R9, discovery is never left stopped (amends R3).** The discovery image is
  pulled when missing before discovery is stopped. If any step after the
  stop fails, the existing `onebox-discovery` container is started again,
  best effort, before the error returns.
- **R10.** The unchanged path also removes stale `.staged-*` directories: a
  committed final marker means Traefik already serves the live directories.
- **R11.** The dead `withdrawnFrom` goes. Recovery gains a test for the surge
  case: the predecessor replica is gone and the interrupted newcomer is the
  only healthy container.
- **Accepted:** after a failed proxy apply, a roll that skips the proxy step
  (`ob secrets push`, recovery) can read a routing document Traefik is not
  serving. Run `ob proxy apply` first; `ob status` shows the proxy as
  drifted. Refusing such rolls outright would also block `ob abort` during
  an incident.
- **Operator check additions:** on each target host, `compose stop` of a
  service leaves a `depends_on` dependent running, and `up -d` of the
  stopped service keeps the dependent's container.
- **Deferred:** an end-to-end assertion that Traefik's container id survives
  a discovery image change. The Compose behavior was verified locally on
  Compose 5.6.0; the release operator check covers the target hosts.

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
