# Zero-Downtime Gaps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the downtime Onebox causes without need: Traefik restarts on every `ob` upgrade and on discovery repair, killed database shutdowns, rolls that retire the last routed replica, and recovery that removes serving containers before restoring.

**Architecture:** The proxy's applied-state marker splits into a `{Config, Compose}` pair, and a pure decision function picks one of three converge paths; only the configuration path restarts Traefik. Managed services get one shutdown grace constant used by render, apply and destroy. Rolling gains two gates that read the discovery controller's published `onebox.yml`: a newcomer must be published before any old replica retires, and a drained replica must be withdrawn before it is stopped. Recovery restores the predecessor before sweeping the interrupted release.

**Tech Stack:** Go 1.27.2, Docker Compose v2, Traefik v3 file provider, `transport.Fake` for unit tests, `virtualClock` (`internal/engine/drain_budget_test.go`) for timeout tests.

**Spec:** `docs/plans/2026-10-17-zero-downtime-gaps.md`

## Global Constraints

- Work on a branch, never `main`. Breaking changes are acceptable; the only compatibility rule is that a legacy one-digest marker takes the configuration path once.
- Marker format: `<config> <compose>` on one line, each `sha256:<64 lowercase hex>`. Written atomically: `printf '%s\n' <marker> > config.hash.tmp && mv -f config.hash.tmp config.hash`. Deleted (`rm -f`) before any path writes to the host proxy directory.
- Publication wait: the existing 30 × 1s loop, now keyed on the output file's inode.
- `app.ServiceStopGrace = 2 * time.Minute`. Rendered as `stop_grace_period: 2m0s`; passed as `--timeout 120` and `docker stop -t 120`.
- `routePublishBudget = 30 * time.Second`. `ConvergeBuffer` stays 3s and stays in place after each gate.
- Routing gates apply only when `Spec.Proxy.Managed` and the workload has routes. Health admission of adopted newcomers applies to every rolling workload.
- An unreadable, failed or unparsable `onebox.yml` read never counts as presence or absence.
- No new host object names. Existing names: `onebox-proxy`, `onebox-discovery`, `/var/lib/onebox/_host/proxy/{config.hash,compose.yaml,config,dynamic}`.
- Gate before every commit: `go build ./... && go vet ./... && go test ./...`. Final task also runs `just check` and `just env-namespace`.
- Commits: conventional (`fix(proxy): …`, `fix(services): …`, `fix(rolling): …`, `fix(recovery): …`), body says why and what an operator sees on rollout.

## Review Focus

1. Output deleted by hand while discovery runs and the marker matches: the converge path must force-recreate discovery, never wait on a controller that will not rewrite. Test in Task 3.
2. Empty, truncated or legacy marker: configuration path, never "unchanged". Tests in Tasks 2 and 3.
3. A workload with one HTTP and one TCP route: published only when its address is a server of both services. Test in Task 7.
4. IPv6-only ingress address: `http://[fd00::5]:3000` matches `fd00::5`. Test in Task 7.
5. `cat onebox.yml` fails during withdrawal: not withdrawn, roll stops, no `docker stop`. Test in Task 9.

---

## Slice 1: Proxy converge

### Task 1: Split proxy identity

**Files:**
- Create: `internal/proxy/identity.go`, `internal/proxy/identity_test.go`
- Modify: `internal/proxy/proxy.go:331-523` (`Stage`, `StageForApp`, `StageForAppManaged`, `stageDefaultManaged`)
- Modify (compile only): `internal/engine/proxy.go:41`, `internal/engine/proxystatus.go:187`, `internal/engine/proxy_test.go:26-49` (`proxyFixture`)

**Interfaces:**
- Produces: `type Identity struct{ Config, Compose string }`; `func (i Identity) String() string`; `func ParseIdentity(s string) (Identity, bool)`. `Stage`, `StageForApp`, `StageForAppManaged` return `(Identity, error)`. `proxyFixture` returns `(*Engine, proxy.Identity, *bytes.Buffer)`.

- [ ] **Step 1: Write the failing tests** in `internal/proxy/identity_test.go`

```go
func TestParseIdentityRoundTrip(t *testing.T) {
	id := Identity{Config: "sha256:" + strings.Repeat("a", 64), Compose: "sha256:" + strings.Repeat("b", 64)}
	got, ok := ParseIdentity(id.String() + "\n")
	if !ok || got != id { t.Fatalf("got %+v %v", got, ok) }
}

func TestParseIdentityRejectsLegacyAndGarbage(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	for _, s := range []string{"", "deadbeef", a, a + " " + a + " " + a, a + " sha256:XYZ", strings.Repeat("a", 64) + " " + a} {
		if _, ok := ParseIdentity(s); ok { t.Errorf("accepted %q", s) }
	}
}

func TestStageSeparatesComposeFromConfig(t *testing.T) {
	// same proxy.config dir staged with DiscoveryImage("v2026.10.4-alpha") and ("v2026.10.5-alpha")
	// want: Config equal, Compose different
}

func TestStageConfigChangeMovesOnlyConfig(t *testing.T) {
	// traefik.yml body changed between two stagings, same discovery image
	// want: Config different, Compose equal
}
```

- [ ] **Step 2: Run** `go test ./internal/proxy -run 'Identity|StageSeparates|StageConfigChange'` — Expected: FAIL, `undefined: Identity`.

- [ ] **Step 3: Implement.** `Config` hashes `newProxyConfigHash()` plus every staged file and the managed dynamic file (today's hash minus the compose bytes). `Compose` is `"sha256:" + hex(sha256(compose))`. Both carry the `sha256:` prefix on both staging paths (today only `stageDefaultManaged` does). Callers compile with `id.String()` until Tasks 3 and 4 replace them; `proxyFixture` and every test that sends `hash + "\n"` send `id.String() + "\n"`.

- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./...` — Expected: PASS.

- [ ] **Step 5: Commit** `refactor(proxy): stage a config digest and a compose digest`

### Task 2: Observe and decide

**Files:**
- Create: `internal/engine/proxy_converge.go`, `internal/engine/proxy_converge_test.go`

**Interfaces:**
- Consumes: `proxy.Identity`, `proxy.ParseIdentity` (Task 1).
- Produces:
  - `type proxyObservation struct { Applied proxy.Identity; AppliedKnown bool; ProxyIDs, DiscoveryIDs []string; OutputInode string }` — `OutputInode` is `""` when `dynamic/onebox.yml` is absent or empty.
  - `type proxyAction int` with `proxyUnchanged`, `proxyConverge` (compose drift or repair, never restarts Traefik), `proxyApply` (configuration changed, restarts Traefik).
  - `func decideProxy(o proxyObservation, staged proxy.Identity) proxyAction`
  - `func (a proxyAction) String() string` → `"unchanged"`, `"update changed containers only, no restart"`, `"restart Traefik (configuration changed)"`.
  - `func (e *Engine) stageProxy(dir string) (proxy.Identity, error)` — the `StageForAppManaged` call now at `internal/engine/proxy.go:30-44`, including `Proxy.Config` resolution against `LocalDir`.
  - `func (e *Engine) observeProxy(ctx context.Context) (proxyObservation, error)` — read-only: `cat` of the marker, `proxyContainerIDs`, `discoveryContainerIDs`, `outputInodeCommand`.
  - `func outputInodeCommand(path string) string` → `if [ -s P ]; then stat -c %i P; fi` with `P` quoted by `q`.

- [ ] **Step 1: Write the failing tests**

```go
func TestDecideProxy(t *testing.T) {
	staged := proxy.Identity{Config: digestA, Compose: digestB}
	running := proxyObservation{Applied: staged, AppliedKnown: true, ProxyIDs: []string{"PX1"}, DiscoveryIDs: []string{"D1"}, OutputInode: "12"}
	// rows: name, mutate(o), want
	// "unknown marker"            AppliedKnown=false            → proxyApply
	// "config drift"              Applied.Config=digestC        → proxyApply
	// "compose drift"             Applied.Compose=digestC       → proxyConverge
	// "traefik missing"           ProxyIDs=nil                  → proxyConverge
	// "discovery missing"         DiscoveryIDs=nil              → proxyConverge
	// "output missing"            OutputInode=""                → proxyConverge
	// "all present and equal"     (none)                        → proxyUnchanged
}

func TestObserveProxyIsReadOnly(t *testing.T) {
	// fake answers marker, both ps queries, inode; after observeProxy no command
	// contains "rm ", "mv ", "mkdir", "up -d", "docker restart" or "> "
}
```

- [ ] **Step 2: Run** `go test ./internal/engine -run 'DecideProxy|ObserveProxy'` — Expected: FAIL, undefined.

- [ ] **Step 3: Implement** the interfaces above. A marker that fails `ParseIdentity` sets `AppliedKnown=false`.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `refactor(proxy): observe the host proxy and decide its converge path`

### Task 3: Converge without needless Traefik restarts

**Files:**
- Modify: `internal/engine/proxy.go:24-232` (`EnsureProxy`)
- Test: `internal/engine/proxy_test.go`

**Interfaces:**
- Consumes: `stageProxy`, `observeProxy`, `decideProxy`, `outputInodeCommand` (Task 2).
- Produces: `func (e *Engine) waitForPublication(ctx context.Context, previousInode string) error` (30 × 1s until the output is non-empty and its inode differs from `previousInode`; error text contains `did not publish` and `docker logs onebox-discovery`); `func (e *Engine) commitProxyIdentity(ctx context.Context, id proxy.Identity) error` (atomic marker write).

- [ ] **Step 1: Verify the Compose assumption** on the local Docker host:

```bash
d=$(mktemp -d) && cd "$d"
cat > compose.yaml <<'EOF'
services:
  a: {image: busybox:1.36, command: sleep 3600}
  b: {image: busybox:1.36, command: sleep 3600, depends_on: {a: {condition: service_started}}}
EOF
docker compose -p obcheck up -d && B1=$(docker compose -p obcheck ps -q b)
sed -i.bak 's/a: {image: busybox:1.36/a: {image: busybox:1.37/' compose.yaml
docker compose -p obcheck up -d a && docker compose -p obcheck up -d b && B2=$(docker compose -p obcheck ps -q b)
docker compose version; test "$B1" = "$B2" && echo SAME || echo RECREATED
docker compose -p obcheck down
```

Expected: `SAME`. On `RECREATED`, stop and report: the converge path's premise fails. Record the `docker compose version` output in the commit body.

- [ ] **Step 2: Write the failing tests** (fakes built on `proxyPS`; marker read is `cat '/var/lib/onebox/_host/proxy/config.hash'`; inode read is `outputInodeCommand('/var/lib/onebox/_host/proxy/dynamic/onebox.yml')`)

```go
func TestEnsureProxyComposeOnlyChangeSkipsSwapAndRestart(t *testing.T) {
	// marker = {staged.Config, other compose}; discovery D1 → D2 after `up -d discovery`; inode "100" → "101"
	// want: contains "/compose.yaml' '/var/lib/onebox/_host/proxy/compose.yaml'"
	// want: not contains "/config' '/var/lib/onebox/_host/proxy/config'", "docker restart", "--force-recreate"
	// want: "mv -f '/var/lib/onebox/_host/proxy/config.hash.tmp'" after the last health inspect
}

func TestEnsureProxyMissingOutputForceRecreatesDiscoveryWithoutRestart(t *testing.T) {
	// marker == staged, both running, inode "" → "200" after force-recreate
	// want: contains "up -d --force-recreate discovery"; not contains "docker restart"
}

func TestEnsureProxyComposePathRejectsStaleOutput(t *testing.T) {
	// discovery id changes, inode stays "100"
	// want: error contains "did not publish"; no "config.hash.tmp" write
}

func TestEnsureProxyMissingMarkerTakesConfigPath(t *testing.T) {
	// marker read returns "" with both containers running and output present
	// want: config swap command present
}
```

Update existing tests:
- `TestEnsureProxyUnchangedIsNoOp`: marker `id.String()`, inode `"12"`.
- `TestEnsureProxyRepairsMissingDiscoveryController` and `TestEnsureProxyRepairsMissingDiscoveryOutput`: also assert no `docker restart` and no config swap.
- `TestEnsureProxyConfigOnlyChangeRestarts`: `rm -f '/var/lib/onebox/_host/proxy/config.hash'` precedes the upload; the marker assertion targets the `config.hash.tmp` `mv`.
- `TestEnsureProxyFailedConvergeLeavesHashUnwritten`: assert the `rm -f` ran and no `config.hash.tmp` write did.

- [ ] **Step 3: Run** `go test ./internal/engine -run EnsureProxy` — Expected: the new tests FAIL.

- [ ] **Step 4: Implement.** After the existing lock, journal and directory preparation, `observeProxy`, then switch on `decideProxy`:
  - `proxyUnchanged`: log `proxy: unchanged and running — not touched`, return.
  - Both other paths: keep today's live/planned compose diff display once, then `rm -f` the marker before any upload.
  - `proxyApply`: today's staged upload and swap, force-recreate discovery, `waitForPublication(ctx, "")` (the swap removed the old output), `up -d proxy`, restart only if the container id did not change, health, `commitProxyIdentity`.
  - `proxyConverge`: upload the staging dir, `mv -f` only its `compose.yaml` over `hp.Compose`, remove the staged dir; `up -d --force-recreate discovery` when `DiscoveryIDs` is empty or `OutputInode` is `""`, else `up -d discovery`; if the discovery id changed or was forced, `waitForPublication(ctx, o.OutputInode)`; `up -d proxy`; if the Traefik id changed or was absent, wait for health; never `docker restart`; `commitProxyIdentity`.

- [ ] **Step 5: Run** the gate — Expected: PASS.

- [ ] **Step 6: Commit** `fix(proxy): restart Traefik only when its configuration changes` — body: `ob` upgrades and discovery repair no longer drop routes; the first apply on each host after upgrading still restarts once (legacy marker); Compose version verified in Step 1.

### Task 4: Status names the drifted half

**Files:**
- Modify: `internal/engine/proxystatus.go:180-200` (local stage via `stageProxy`), `:280-303` (render), `internal/engine/status_snapshot.go:450-460`
- Test: `internal/engine/proxystatus_test.go`, status snapshot tests

**Interfaces:**
- Consumes: `stageProxy` (Task 2), `proxy.ParseIdentity` (Task 1).
- Produces: `func proxyDriftParts(applied string, local proxy.Identity) []string` → `nil` in sync, else `["config"]`, `["compose"]` or `["config", "compose"]`; an unparsable marker yields both. `func shortDigest(d string) string` → first 8 hex characters after `sha256:`.

- [ ] **Step 1: Write the failing tests**

```go
func TestProxyDriftParts(t *testing.T) {
	// equal → nil; compose differs → ["compose"]; config differs → ["config"];
	// "deadbeef" → ["config","compose"]
}

func TestProxyStatusNamesComposeDrift(t *testing.T) {
	// applied = {local.Config, other}.String()
	// output contains "DRIFTED ⚠ (compose)" and not "(config"
}
```

- [ ] **Step 2: Run** `go test ./internal/engine -run 'ProxyDrift|ProxyStatus'` — Expected: FAIL.

- [ ] **Step 3: Implement.** In sync: `config <shortDigest> compose <shortDigest> (in sync)`. Drift: `DRIFTED ⚠ (<parts joined by ", ">) — `ob proxy apply``. The JSON snapshot keeps `LocalConfigHash` (now `local.String()`) and its applied field, and decides divergence with `proxyDriftParts`.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `fix(status): say which half of the proxy drifted`

---

## Slice 2: Plan visibility

### Task 5: The deploy plan names the proxy action

**Files:**
- Modify: `internal/engine/proxy_converge.go`, `internal/onebox/execution_types.go:53-62`, `internal/onebox/plan_deploy.go` (after `e.Refresh`), `cmd/ob/commands.go:323-337` (`renderDeployPlan`)
- Test: `internal/engine/proxy_converge_test.go`, `cmd/ob/commands_test.go`

**Interfaces:**
- Consumes: `stageProxy`, `observeProxy`, `decideProxy` (Task 2).
- Produces: `func (e *Engine) PlanProxy(ctx context.Context) (string, error)` → `""` when the proxy is not managed, else `decideProxy(...).String()`. `DeployPlan.ProxyAction string` with tag `json:"proxy_action,omitempty"`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPlanProxyReportsEachAction(t *testing.T) {
	// unchanged → "unchanged"; compose drift → "update changed containers only, no restart";
	// config drift → "restart Traefik (configuration changed)"; unmanaged → ""
	// every case: no mutating command (as in TestObserveProxyIsReadOnly)
}

func TestRenderDeployPlanShowsProxyAction(t *testing.T) {
	// plan with ProxyAction "restart Traefik (configuration changed)"
	// output contains "proxy: restart Traefik (configuration changed)"
}
```

- [ ] **Step 2: Run** `go test ./internal/engine ./cmd/ob -run 'PlanProxy|ProxyAction'` — Expected: FAIL.

- [ ] **Step 3: Implement.** `PlanDeploy` sets `ProxyAction` before the plan digest is computed, so the digest covers it. `renderDeployPlan` prints `proxy: <action>` with `u.Infof` right after the diff / no-op block.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `feat(plan): show what a deploy does to the proxy`

---

## Slice 3: Managed services

### Task 6: A clean shutdown window

**Files:**
- Modify: `internal/app/services.go:367-380` (render), `internal/engine/services.go:141` (apply), `internal/engine/services.go:497` (`removeServices`)
- Modify: `internal/app/testdata/contract-verdicts.json` (regenerated), `site/src/content/docs/guides/add-a-database.mdx`
- Test: `internal/app/services_test.go`; create `internal/engine/services_test.go`

**Interfaces:**
- Produces: `app.ServiceStopGrace = 2 * time.Minute`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRenderedServicesCarryStopGrace(t *testing.T) {
	// postgres and redis services rendered: each document has stop_grace_period: 2m0s
}

func TestApplyServicesPassesStopTimeout(t *testing.T) {
	// commands contain "up -d --remove-orphans --timeout 120"
}

func TestRemoveServicesStopsGracefullyBeforeRemoval(t *testing.T) {
	// for container SVC1: command contains "docker stop -t 120 SVC1 >/dev/null 2>&1; docker rm -f SVC1"
}
```

- [ ] **Step 2: Run** `go test ./internal/app ./internal/engine -run 'StopGrace|StopTimeout|StopsGracefully'` — Expected: FAIL.

- [ ] **Step 3: Implement.** Render `"stop_grace_period": ServiceStopGrace.String()`. Apply and destroy derive 120 from `int(app.ServiceStopGrace.Seconds())`. Destroy's `rm -f` result still decides success; the stop is best effort.

- [ ] **Step 4: Regenerate goldens.** Run `ONEBOX_UPDATE_VERDICTS=1 go test ./internal/app -run TestContractDidNotMove`, then `git diff --stat internal/app/testdata` — Expected: only `contract-verdicts.json` changes.

- [ ] **Step 5: Docs.** In `add-a-database.mdx`, beside the `/dev/shm` paragraph: services get up to 2 minutes to shut down cleanly; the first `ob service apply` after upgrading recreates every service, and deploy does so for PostgreSQL services with declared extensions.

- [ ] **Step 6: Run** the gate — Expected: PASS.

- [ ] **Step 7: Commit** `fix(services): give managed services two minutes to shut down` — body: a 10s kill forced crash recovery or AOF replay; rollout recreates each service once, as with #220.

---

## Slice 4: Rolling routing gates

### Task 7: Read published routing

**Files:**
- Modify: `internal/discovery/docker.go:102-107`
- Create: `internal/engine/routing.go`, `internal/engine/routing_test.go`
- Test: `internal/discovery/discovery_test.go`

**Interfaces:**
- Produces:
  - `func discovery.EndpointAddress(ipv4, ipv6 string) string` — `ipv4` when it parses as an IP, else `ipv6`; `inspect` uses it.
  - `const routePublishBudget = 30 * time.Second`
  - `func (e *Engine) routeGated(role app.Workload) bool`
  - `func routedIn(doc discovery.Dynamic, n app.Names, workload string, role app.Workload, address string) bool` — address is a server of every expected service.
  - `func withdrawnFrom(doc discovery.Dynamic, n app.Names, workload string, role app.Workload, address string) bool` — address is a server of none.
  - Expected services: for route `i`, `n.ProxyServiceFor(workload, i)`, under `TCP.Services` when `Protocol == "tcp"`, else `HTTP.Services`. Hosts compare by `net.ParseIP(...).Equal`; HTTP hosts come from `url.Parse(URL).Hostname()`, TCP hosts from `net.SplitHostPort(Address)`.
  - `func (e *Engine) routeAddress(ctx context.Context, id string) (string, error)` — `docker inspect -f '{{with index .NetworkSettings.Networks "<Spec.Proxy.Network>"}}{{.IPAddress}} {{.GlobalIPv6Address}}{{end}}' <id>`, through `EndpointAddress`; empty is an error naming the network.
  - `func (e *Engine) readPublishedRoutes(ctx context.Context) (discovery.Dynamic, error)` — `cat` of `proxy.HostPaths(e.names()).Dynamic + "/onebox.yml"`; non-zero exit or YAML error is an error.
  - `func (e *Engine) awaitRouting(ctx context.Context, workload string, role app.Workload, id string, routed bool, pollEvery time.Duration) error` — resolves the address once, polls until `routedIn` (routed) or `withdrawnFrom` (not routed) holds, deadline `e.Opts.Now() + routePublishBudget`. Errors: routed → contains `did not publish` and `docker logs onebox-discovery`; withdrawn → contains `still routes` and `ob resume`; both include the last read error when there was one.

- [ ] **Step 1: Write the failing tests**

```go
func TestEndpointAddressPrefersIPv4(t *testing.T)        // ("10.0.0.5","fd00::5") → "10.0.0.5"; ("","fd00::5") → "fd00::5"
func TestRoutedInRequiresEveryExpectedService(t *testing.T) // web: http:3000 + tcp:5432; only the HTTP server lists 10.0.0.5 → false; add TCP "10.0.0.5:5432" → true
func TestRoutedInMatchesIPv6(t *testing.T)               // "http://[fd00::5]:3000" vs "fd00::5" → true
func TestWithdrawnFromEveryService(t *testing.T)         // absent everywhere → true; in one service → false
func TestRouteGatedOnlyForManagedProxyWithRoutes(t *testing.T)
func TestAwaitRoutingTreatsReadFailureAsNeither(t *testing.T) // cat exits 1 every time; virtualClock; both modes return errors, never nil
```

- [ ] **Step 2: Run** `go test ./internal/discovery ./internal/engine -run 'EndpointAddress|RoutedIn|WithdrawnFrom|RouteGated|AwaitRouting'` — Expected: FAIL.

- [ ] **Step 3: Implement** the interfaces above.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `feat(rolling): read the routes discovery publishes`

### Task 8: Admit newcomers before retiring anything

**Files:**
- Modify: `internal/engine/roll.go:132-258` (`rollRoleForRelease`)
- Test: `internal/engine/roll_test.go`

**Interfaces:**
- Consumes: `routeGated`, `awaitRouting` (Task 7).
- Produces: `func (e *Engine) admitNewcomers(ctx context.Context, workload string, role app.Workload, ids []string, within, pollEvery time.Duration) error` — each id must pass `waitHealth(id, "healthy", within, pollEvery)`, then `awaitRouting(..., true, ...)` when gated. It never removes a container.

- [ ] **Step 1: Write the failing tests.** Add a helper `routedRollFake(base *transport.Fake, publishFrom map[string]int)` that answers the `routeAddress` inspect with `10.0.0.<n>` per id and the `onebox.yml` read with a document listing each id from its N-th read on; the config sets `Proxy.Managed = true` and one HTTP route on `web`.

```go
func TestRollRetiresOldOnlyAfterNewcomerPublished(t *testing.T)   // NEW1 published from read 3: "docker exec OLD1 … touch" comes after the 3rd onebox.yml read
func TestRollRemovesNewcomerNeverPublished(t *testing.T)          // never published: error contains "did not publish"; "docker rm -f NEW1" present; no "docker exec OLD1"
func TestRollResumeAdmitsAdoptedNewcomerBeforeRetiring(t *testing.T) // resume=true, NEW1 health stays "starting": error; no "docker exec OLD1"; no "docker rm -f NEW1"
func TestRollUnmanagedProxyReadsNoRoutes(t *testing.T)            // testConfig(): no command reads onebox.yml
```

- [ ] **Step 2: Run** `go test ./internal/engine -run 'TestRoll(RetiresOld|RemovesNewcomer|ResumeAdmits|UnmanagedProxy)'` — Expected: FAIL.

- [ ] **Step 3: Implement.** In the surge block, after `waitHealth` succeeds and when gated, `awaitRouting` the new id; on failure remove it exactly as the health failure does (`roll.go:233-235`). Call `admitNewcomers` once per pass immediately before retiring a surplus (`news[:desired]`), before retiring an old (`news`), and before the success `break` (`news`).

- [ ] **Step 4: Run** the gate — Expected: PASS, including `TestRoll*` resume adoption (`roll_test.go:174`).

- [ ] **Step 5: Commit** `fix(rolling): never retire a replica before its replacement is routed`

### Task 9: Withdraw before stopping

**Files:**
- Modify: `internal/engine/roll.go:260-312` (`retireContainer`), callers `roll.go:169`, `roll.go:242`, `internal/engine/retired_workloads.go:43`
- Test: `internal/engine/roll_test.go`

**Interfaces:**
- Consumes: `routeGated`, `awaitRouting` (Task 7).
- Produces: `retireContainer(ctx context.Context, workload string, role app.Workload, id string, pollEvery time.Duration) error`.

- [ ] **Step 1: Write the failing tests** (call `retireContainer` directly on a routed fake)

```go
func TestRetireWaitsForWithdrawalBeforeStop(t *testing.T)       // OLD1 listed for 2 reads, then absent: "docker stop -t" OLD1 after the 3rd read
func TestRetireStopsWhenWithdrawalTimesOut(t *testing.T)        // always listed, virtualClock: error contains "still routes"; no "docker stop" OLD1
func TestRetireUnreadableRoutesAreNotWithdrawal(t *testing.T)   // cat exits 1 every time: error; no "docker stop" OLD1
func TestRetireUnguardedHealthcheckSkipsWithdrawalGate(t *testing.T) // exec-form healthcheck: no onebox.yml read; stop proceeds after the buffers
```

- [ ] **Step 2: Run** `go test ./internal/engine -run TestRetire` — Expected: FAIL.

- [ ] **Step 3: Implement.** In the guarded branch, after the drain flip wait and before the second `ConvergeBuffer` sleep, `awaitRouting(ctx, workload, role, id, false, pollEvery)` when gated; return its error. The unguarded branch is unchanged.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `fix(rolling): stop a replica only after the proxy has dropped it`

---

## Slice 5: Recovery

### Task 10: Restore before removing serving containers

**Files:**
- Modify: `internal/engine/recovery.go:169-186` (`recoverInterrupted` order), add `removeInterruptedNonServing` beside `removeExactReleaseContainers`
- Test: `internal/engine/recovery_test.go`

**Interfaces:**
- Consumes: `admitNewcomers` (Task 8) through `RollRole`.
- Produces: `func (e *Engine) removeInterruptedNonServing(ctx context.Context, interruptedID string, previous *Engine) error`. Lists `docker ps -a --filter label=onebox.app=<app> --filter label=onebox.release=<id> --format '{{.ID}}|{{.Label "com.docker.compose.service"}}|{{.Names}}|{{.State}}'`. Keeps a container only when `previous != nil`, its state is `running`, `previous.Spec.Workloads[service]` exists with `Mode() == "rolling"`, and its name is not `e.names().TransientContainer(service)`. Removes the rest with the existing guarded `docker rm -f` command.

- [ ] **Step 1: Write the failing tests**

```go
func TestRecoveryRollsPredecessorBeforeRemovingServingContainer(t *testing.T) {
	// interrupted release owns NEW1 (running, service web, slot name)
	// want: first "--scale web=" precedes every command that stops or removes NEW1
}

func TestRecoveryWithoutPredecessorRemovesEverythingFirst(t *testing.T) {
	// previous == nil: every interrupted container removed before finalization, as today
}
```

Update `TestRecoveryRetryKeepsCheckpointUntilHealthyAndSweepsStaleRoles` so its fake answers the new listing format: `STALE1` (service not in the predecessor) is removed before restore; `NEW1` is retired by the roll or swept after it. Its verify-phase and checkpoint assertions stay.

- [ ] **Step 2: Run** `go test ./internal/engine -run TestRecovery` — Expected: the new tests FAIL.

Also, decided during implementation: `restoreReleaseRoles(ctx context.Context, previous *Engine, previousID, interruptedID string) error` retains a workload only when `retainableWorkload(...)` holds and no `observed[role]` container has `release == interruptedID`.

```go
func TestRestoreReleaseRolesRollsInterruptedOwnedMatchingRevision(t *testing.T) {
	// web's only container carries the predecessor's revision but the interrupted release label
	// want: a "--scale web=" surge, and that container drained and retired by the roll, not retained
}
```

`TestRestoreReleaseRolesRetainsMatchingTargetRevisions` passes the new argument and keeps its assertions.

- [ ] **Step 3: Implement** the order: `removeInterruptedNonServing` (phase `cleanup`), `restoreReleaseRoles` (phase `restore`), `removeExactReleaseContainers` (phase `sweep`), `Verify` (phase `verify`), `assertNoReleaseContainers` (phase `cleanup-postcondition`), then finalization unchanged.

- [ ] **Step 4: Run** the gate — Expected: PASS.

- [ ] **Step 5: Commit** `fix(recovery): restore the predecessor before removing serving containers`

---

### Task 11: Docs and final gates

**Files:**
- Modify: `site/src/content/docs/explanation/safety-envelope.mdx` (rolling and proxy paragraphs), `docs/plans/2026-10-17-zero-downtime-gaps.md` (status: implemented)

- [ ] **Step 1: Docs.** In `safety-envelope.mdx`: a roll retires a replica only after discovery has published its replacement and stops it only after the proxy has dropped it; Traefik restarts only when its configuration changes, not on `ob` upgrades; the plan's `proxy:` line says which.

- [ ] **Step 2: Run** `just check && just env-namespace` — Expected: PASS. If `site-build` reports missing packages, run `just site-install` first.

- [ ] **Step 3: Commit** `docs: describe the routing gates and when Traefik restarts`

- [ ] **Step 4: Operator check before release.** Record `docker compose version` on each target host. If any is older than the version recorded in Task 3 Step 1, rerun that script there before releasing. Also check, on each host, that `docker compose stop a` leaves a `depends_on` dependent `b` running and that `up -d a` keeps `b`'s container.

---

## Slice 6: Review round (spec §6)

Same gate and commit discipline. Every existing assertion stays; a test that the review found vacuous is tightened, never deleted.

### Task 12: Withdrawal that survives route changes (R1)

**Files:** `internal/engine/routing.go`, `internal/engine/roll.go:295-340`, `internal/engine/roll_test.go`, `internal/engine/routing_test.go`

**Interfaces:**
- `func withdrawnFromAll(doc discovery.Dynamic, address string) bool` — address is a server of no HTTP and no TCP service in the document. Replaces `withdrawnFrom` in the withdrawal gate.
- `routeAddress` returns `("", nil)` when the container has no address on the proxy network; the routed gate still treats that as an error.
- `retireContainer` gates withdrawal on `e.Spec.Proxy.Managed` alone.

- [ ] **Tests:**

```go
func TestRetireOldReplicaOffProxyNetworkIsWithdrawn(t *testing.T)   // inspect prints " ": stop proceeds; no onebox.yml wait
func TestRetireWaitsWhileOldReplicaServesARemovedRoute(t *testing.T) // new role has no routes; OLD1 listed under onebox-web for 2 reads: stop after the 3rd
func TestRetireWaitsWhileOldReplicaServesAnotherProtocol(t *testing.T) // new role http; OLD1 listed under a TCP service: waits, then error on timeout
func TestRollResumeBlocksAdoptedHealthyUnpublishedNewcomer(t *testing.T) // managed proxy, adopted NEW1 healthy, never published: error; no "docker exec OLD1"
func TestRollWithdrawsOldReplicaInsideFullRoll(t *testing.T)        // routedRollFake lists OLD1 until drained: "docker stop" OLD1 after the read without it
```

- [ ] **Commit** `fix(rolling): withdraw old replicas whatever the new routes are`

### Task 13: Converge without a restart on retry, with fresh evidence (R2, R3, R5, R6)

**Files:** `internal/engine/proxy.go:85-230`, `internal/engine/proxy_test.go`

**Interfaces:**
- `const pendingComposeDigest = "sha256:" + 64 × "0"`; the converge path calls `commitProxyIdentity(ctx, proxy.Identity{Config: o.Applied.Config, Compose: pendingComposeDigest})` where it now deletes the marker.
- Converge path: upload a staging dir holding only `compose.yaml`; `compose stop discovery`; sample `outputInodeCommand`; `up -d discovery` (with `--force-recreate` when discovery or its output was missing); `waitForPublication(ctx, sampledInode)`; `up -d proxy`; no restart.
- Config path swap: move live `config` and `dynamic` (when present) into `<staged>/retired/`, then move the staged ones in; no `rm -rf` of live directories. The staged dir (with `retired/`) is removed by the deferred cleanup only when the apply succeeded.

- [ ] **Tests** (and tighten the review's vacuous ones):

```go
func TestEnsureProxyFailedConvergeRetriesAsConverge(t *testing.T) // converge fails at "up -d discovery"; marker holds "<config> sha256:000…"; second run: no config swap, no "docker restart"
func TestEnsureProxyConvergeStopsDiscoveryBeforeSampling(t *testing.T) // "stop discovery" precedes the inode sample, which precedes "up -d discovery"
func TestEnsureProxyConvergeUploadsOnlyCompose(t *testing.T)    // the uploaded dir holds compose.yaml only
func TestEnsureProxyConfigSwapRetiresLiveDirsInsteadOfDeleting(t *testing.T) // no "rm -rf '/var/lib/onebox/_host/proxy/config'"; "mv … /retired/config" present
```

Tighten: `TestEnsureProxyComposeOnlyChangeSkipsSwapAndRestart` asserts the marker write after the last discovery publication wait (not after a `docker inspect` that path never runs); `TestEnsureProxyMissingOutputForceRecreatesDiscoveryWithoutRestart` returns `""` before and a new inode after; `proxyPS` answers the discovery query with its own id.

- [ ] **Commit** `fix(proxy): retry a converge as a converge and keep routes until restart`

### Task 14: Name a Traefik replacement in the plan (R4)

**Files:** `internal/engine/proxy_converge.go`, `internal/engine/proxy.go`, `internal/engine/proxy_converge_test.go`

**Interfaces:**
- `func traefikDefinitionChanged(live, rendered []byte) bool` — parse both as YAML; compare `services.proxy` and top-level `networks` with `reflect.DeepEqual`; parse error or empty live → `true`.
- `observeProxy(ctx context.Context, rendered []byte) (proxyObservation, error)` also reads the live `compose.yaml` and sets `TraefikChanged bool`.
- New action `proxyReplace`, chosen by `decideProxy` when it would choose `proxyConverge` and `TraefikChanged` is true. `EnsureProxy` runs it exactly as `proxyConverge`. `String()` → `"recreate Traefik (its container definition changed)"`.
- `func proxyPlanLine(o proxyObservation, a proxyAction) string` → `"start Traefik"` when `len(o.ProxyIDs) == 0`, else `a.String()`. `PlanProxy` returns it.

- [ ] **Tests:**

```go
func TestTraefikDefinitionChanged(t *testing.T) // discovery image only → false; traefik image → true; ports → true; unparsable live → true
func TestDecideProxyNamesTraefikReplacement(t *testing.T) // compose drift + TraefikChanged → proxyReplace
func TestPlanProxySaysStartWhenTraefikMissing(t *testing.T)
```

- [ ] **Commit** `fix(plan): say when Traefik itself will be replaced`

### Task 15: Single-replica recovery keeps serving (R7)

**Files:** `internal/engine/recovery.go`, `internal/engine/recovery_test.go`

**Interfaces:**
- In `removeInterruptedNonServing`: a running container of a rolling predecessor workload named `TransientContainer(service)` is renamed with `renameContainer` to the lowest `Container(service, i)`, `i ≥ 1`, that no container (any state) holds; it is then kept like any other serving container.
- `func (e *Engine) undrainPredecessorReplicas(ctx context.Context, previous *Engine, previousID string) error` — for each running container labelled `onebox.release=previousID` of a rolling workload, `docker exec <id> rm -f /tmp/onebox-drain` (`app.DrainFile`), best effort; called right before `restoreReleaseRoles`.

- [ ] **Tests:**

```go
func TestRecoverySingleReplicaKeepsInterruptedNewcomerServing(t *testing.T)
// replicas: 1. NEW1 (interrupted release) holds the transient name and is healthy; OLD1 (predecessor) is running and drained.
// want, in order: NEW1 renamed to the free slot (never `docker rm -f NEW1` before OLD1 is admitted);
// `docker exec OLD1 rm -f /tmp/onebox-drain`; OLD1 adopted and admitted (healthy, and published when the proxy is managed);
// only then NEW1 drained, stopped and removed. No surge: the predecessor's own replica is adopted.
```

Corrected during implementation: the predecessor's running replica is adopted, not replaced, so the roll does not surge.

Fix `TestRecoveryRollsPredecessorBeforeRemovingServingContainer` to use the fake's own `OLD1` listing and assert touch → stop → rm of `OLD1` with `NEW1` surviving.

- [ ] **Commit** `fix(recovery): keep a single-replica workload serving through recovery`

### Task 16: Docs for the review round

**Files:** `site/src/content/docs/explanation/safety-envelope.mdx`

- [ ] Correct the proxy sentence: Traefik restarts when its configuration changes and is recreated when its own image or ports change; `ob` upgrades and discovery repair do neither otherwise; the plan's `proxy:` line says which. State the assumption that Traefik loads published routes within the 3s converge buffer. State that the first proxy apply after upgrading restarts Traefik once and the first `ob service apply` recreates each managed service once.
- [ ] **Commit** `docs: describe proxy actions and the one-time upgrade restarts`

### Task 17: Second review round (spec §6 R8-R11)

**Files:** `internal/engine/proxy.go`, `internal/engine/proxy_converge.go`, `internal/engine/routing.go`, `internal/engine/recovery_test.go`, and their tests

**Interfaces:**
- `const pendingReplaceDigest = "sha256:" + 63 × "0" + "1"`. The replace path commits `{Config: o.Applied.Config, Compose: pendingReplaceDigest}`; the converge path keeps `pendingComposeDigest`. `observeProxy` sets `TraefikChanged = true` when `Applied.Compose == pendingReplaceDigest`.
- Before `compose stop discovery`: `docker image inspect <image> >/dev/null 2>&1 || docker pull <image>` with `<image> = proxy.DiscoveryImage(e.Opts.Runner.Version)`; a failed pull returns before the stop.
- After the stop, any returned error first runs `docker start onebox-discovery` (best effort; its result is logged, not returned).
- The `proxyUnchanged` branch runs the same stale `.staged-*` cleanup the success path runs.
- Remove `withdrawnFrom` and its test.

- [ ] **Tests:**

```go
func TestEnsureProxyFailedReplaceIsPlannedAsReplace(t *testing.T) // replace fails at "up -d discovery": marker holds pendingReplaceDigest; PlanProxy then says "recreate Traefik (its container definition changed)"
func TestEnsureProxyPullsDiscoveryBeforeStopping(t *testing.T)    // image missing: "docker pull" precedes "stop discovery"; failed pull: no "stop discovery"
func TestEnsureProxyRestartsDiscoveryAfterFailedUp(t *testing.T)  // "up -d discovery" fails: "docker start onebox-discovery" runs before the error returns
func TestEnsureProxyUnchangedCleansStaleStaging(t *testing.T)     // unchanged path runs the ".staged-*" cleanup
func TestRecoverySurgesWhenOnlyInterruptedNewcomerServes(t *testing.T) // replicas: 1, no predecessor replica, NEW1 holds the transient name: NEW1 renamed to slot 1, predecessor surge takes the transient name, then NEW1 drained, stopped, removed
```

- [ ] **Gate:** `go build ./... && go vet ./... && go test -p 2 ./...`; `golangci-lint run ./...` 0 issues.
- [ ] **Commit** `fix(proxy): never leave discovery stopped and keep a failed replace labelled`

### Task 18: Code review round (spec §6 R12-R17)

**Files:** `internal/engine/proxy.go`, `internal/engine/proxy_converge.go`, `internal/engine/roll.go`, `internal/engine/routing.go`, the deploy execution path that sets engine options from a sealed plan (`internal/onebox/execute.go`, beside `options.WorkloadPlans`), and their tests

**Interfaces:**
- R12: the apply path runs the same `docker image inspect <image> >/dev/null 2>&1 || docker pull <image>` before `up -d --force-recreate discovery` and sets `discoveryStopped = true` before it. The deferred fallback runs `docker start onebox-discovery`; when that exits non-zero, `docker compose -p onebox-proxy -f <live compose.yaml> up -d discovery`. Both best effort, logged.
- R13: engine option `ExpectedProxyAction string`; set from `plan.ProxyAction` when executing a sealed deploy plan. When non-empty, `EnsureProxy` computes `proxyPlanLine(o, action)` after `observeProxy` and returns a drift error naming both lines when they differ, before any mutation.
- R14: in `observeProxy`, when `len(ProxyIDs) > 0`: `docker inspect -f '{{.Image}}' <proxy id>` versus `docker image inspect -f '{{.Id}}' <configured image>` (the image `RenderComposeForApp` uses); differ or tag missing → `TraefikChanged = true`.
- R15: before observing, remove `.staged-*` directories that contain no `retired/`.
- R16: `rollRoleForRelease` keeps an `admitted map[string]bool`; `admitNewcomers` skips ids already admitted in this invocation and records the ones it admits.
- R17: routing error text uses `proxy.DiscoveryContainerName`.

- [ ] **Tests:**

```go
func TestEnsureProxyApplyRestoresDiscoveryWhenRecreateFails(t *testing.T) // force-recreate fails, "docker start" exits 1: "compose … up -d discovery" fallback runs
func TestEnsureProxyApplyPullsDiscoveryBeforeRecreate(t *testing.T)
func TestEnsureProxyRefusesPlannedActionDrift(t *testing.T)             // ExpectedProxyAction "unchanged", observation says restart: error, no mutation after observe
func TestObserveProxyFlagsRepulledTraefikImage(t *testing.T)            // container image ID ≠ tag ID → TraefikChanged; equal → false (definition otherwise equal)
func TestEnsureProxyRemovesFailedUploadsButKeepsRetired(t *testing.T)
func TestRollAdmitsEachNewcomerOnce(t *testing.T)                       // replicas: 3: each newcomer's health inspect for admission runs once
```

Plus one execution-path test that a sealed plan's `ProxyAction` reaches `ExpectedProxyAction`.

- [ ] **Gate:** `go build ./... && go vet ./... && go test -p 2 ./...`; `golangci-lint run ./...` 0 issues.
- [ ] **Commit(s)**: `fix(proxy): restore discovery on every path and bind the planned proxy action`, `perf(rolling): admit each newcomer once`.
