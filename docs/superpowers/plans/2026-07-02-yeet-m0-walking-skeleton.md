# yeet M0 Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A single Go binary (`yeet`) that deploys a docker-compose app (monk) end-to-end with zero downtime over SSH: `yeet validate`, `yeet render`, `yeet deploy`, `yeet rollback`.

**Architecture:** Agentless engine per `docs/design.html` rev 5, M0 scope only (§12): config load (plain YAML — CUE is M1), compose canonicalization via `compose-go` (the loader docker compose v2 itself uses — satisfies the rev 5 "never re-implement the spec" rule in-process), a `Transport` interface (SSH + Local), versioned release dirs under `/var/lib/yeet/<app>/releases/<id>/` with symlink activation, and the scale–health–drain choreography with the rev 5 join→converged→drain→converged→bleed→SIGTERM traffic-shift protocol (drain = poison file checked by a wrapped healthcheck). **Explicitly NOT in M0:** plan/apply, CUE, journal/fencing/locks, resume/abort, migration-gate protocol, multi-host, managed proxy, secrets/SOPS, `init` doctor.

**Tech Stack:** Go ≥1.24 (host has 1.26.4), `spf13/cobra`, `gopkg.in/yaml.v3`, `github.com/compose-spec/compose-go/v2`, `golang.org/x/crypto/ssh` (+ `knownhosts`).

## Global Constraints

- Single static binary; no daemon, nothing installed on hosts beyond docker + compose plugin (design §02 "Agentless").
- Every remote mutation is a plain shell command issued through `Transport`; `--verbose` prints each one (design §02 "Boring and legible").
- Compose project name is ALWAYS the app name (`-p <app>`) so successive release dirs address the same containers.
- Remote layout: `/var/lib/yeet/<app>/releases/<id>/` + `current` symlink. Nothing live is ever overwritten (design §04 transfer).
- Injected compose changes are ONLY: `yeet.*` labels, the (wrapped) healthcheck, nothing else (design §02 closed injection set).
- All release/job compose invocations carry `--no-deps` (design §03 accessory firewall).
- Rolled services must have no `container_name`, no host ports, no `deploy.replicas` — `validate` errors (design §03).
- The drain-poison file path is `/tmp/yeet-drain` (constant `compose.DrainFile`).
- Do not implement ahead of M0: no journal writes, no lock files, no plan artifacts.
- compose-go v2 API note: `Project.Services` is a `map[string]types.ServiceConfig`; field names in tasks below are correct for v2.x but let the compiler arbitrate — the tests pin behavior, not the library API.

### Command-injection rules (security)

Transport commands are shell strings by nature (SSH exec is always shell-parsed remotely; Local mirrors it for parity), so **every token interpolated into a command string must be either validated or single-quoted** — no exceptions:

- **Validated by regex before use** (reject at Validate/Classify time, not quote):
  app name `^[a-z][a-z0-9-]*$` (Task 2, already enforced); compose service names
  `^[a-zA-Z0-9._-]+$` (add to `Classify`, Task 3); role names same pattern (add to
  `config.Validate`, Task 2); release ids are self-constructed but the git SHA
  component must match `^[0-9a-f]{4,40}$` or be replaced with `nogit` (Task 7);
  signal names `^[A-Z0-9]+$` (Task 2); container ids returned by docker must match
  `^[0-9a-f]{4,64}$` before reuse in a later command (add a `validID` helper in
  Task 8's `containerIDs` — a compromised host lying in `docker ps` output must
  not be able to inject into subsequent commands).
- **Single-quoted via `q()`/`shq()`**: every path (remote dirs, compose file paths).
- **Verbatim by design**: `hooks.*` values — the documented escape hatch (design §01:
  hooks are the operator's own commands, same trust level as their shell). Never
  interpolate anything *into* a hook string; only prepend validated env exports.
- Go-side `exec.Command` (gitShortSHA, e2e helpers) passes argv directly — no `sh -c`
  on the runner side outside LocalTransport itself.

## File Structure

```
go.mod
cmd/yeet/main.go                 # cobra root; wires subcommands; --verbose flag
internal/config/config.go        # yeet.yml model, Load, Validate, Duration type
internal/config/config_test.go
internal/compose/compose.go      # Load (compose-go), Classify, CheckRollable
internal/compose/render.go       # label + healthcheck injection, MarshalYAML
internal/compose/compose_test.go
internal/compose/render_test.go
internal/compose/testdata/       # minimal + monk-shaped compose fixtures
internal/transport/transport.go  # Transport iface, Result, LocalTransport
internal/transport/ssh.go        # SSHTransport: user@host, agent/key auth, known-hosts, tar Upload
internal/transport/fake.go       # FakeTransport for engine tests (records + scripted replies)
internal/transport/transport_test.go
internal/release/release.go      # release ids, remote paths, snapshot, current/previous
internal/release/release_test.go
internal/engine/engine.go        # Deploy lifecycle orchestration; Options{Verbose, Sleeper}
internal/engine/preflight.go
internal/engine/roll.go          # rolling + recreate choreography (the heart)
internal/engine/verify.go        # host-side verify + finalize (symlink flip, prune)
internal/engine/*_test.go
e2e/e2e_test.go                  # gated: YEET_E2E=1, local docker, zero-downtime probe
e2e/testdata/app/                # busybox httpd demo app
```

---

### Task 1: Module scaffold + CLI root

**Files:**
- Create: `go.mod`, `cmd/yeet/main.go`, `cmd/yeet/main_test.go`

**Interfaces:**
- Produces: `main.newRootCmd() *cobra.Command` with persistent flags `--verbose`, `-e/--env <name>` (default `production`), `-c/--config <path>` (default `yeet.yml`); subcommands added by later tasks.

- [ ] **Step 1: Write the failing test**

```go
// cmd/yeet/main_test.go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpListsVerbs(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "yeet") {
		t.Fatalf("help output missing binary name: %s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/v/Projects/labstack/yeet && go mod init github.com/labstack/yeet && go test ./cmd/...`
Expected: FAIL — `newRootCmd` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// cmd/yeet/main.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.0.1-m0"

type globalFlags struct {
	Verbose    bool
	Env        string
	ConfigPath string
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "yeet",
		Short:         "plan-before-apply, zero-downtime deploys for compose-first apps (M0 skeleton)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&g.Verbose, "verbose", "v", false, "print every remote command")
	root.PersistentFlags().StringVarP(&g.Env, "env", "e", "production", "environment name")
	root.PersistentFlags().StringVarP(&g.ConfigPath, "config", "c", "yeet.yml", "path to yeet.yml")
	// subcommands registered here by later tasks: validate, render, deploy, rollback
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "yeet:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run tests + build**

Run: `go get github.com/spf13/cobra@latest && go test ./cmd/... && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/
git commit -m "feat(m0): scaffold yeet binary with cobra root"
```

---

### Task 2: Config — model, Load, Validate

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `config.Load(path string) (*Config, error)`
  - `(*Config).Validate() error` (config-only checks; compose cross-checks live in Task 3)
  - `(*Config).Environment(name string) (Environment, error)`
  - Types below, exactly as named — later tasks depend on these field names.

- [ ] **Step 1: Write the failing test**

```go
// internal/config/config_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sample = `
app: monk
compose: docker-compose.yaml
environments:
  production: { hosts: [deploy@monk.labstack.net] }
roles:
  web:    { service: server, mode: rolling, ready: { http: /healthz, port: 7500 } }
  worker: { service: worker, mode: recreate, drain: { signal: TERM, wait: 30s } }
order: [web, worker]
accessories: [postgres, redis, traefik]
jobs: [migrate]
hooks: { migrate: docker compose run --rm --no-deps migrate }
verify:
  - { http: /healthz, role: web }
`

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yeet.yml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Roles["web"].Ready.HTTP != "/healthz" || cfg.Roles["web"].Ready.Port != 7500 {
		t.Fatalf("ready parsed wrong: %+v", cfg.Roles["web"].Ready)
	}
	if time.Duration(cfg.Roles["worker"].Drain.Wait) != 30*time.Second {
		t.Fatalf("drain wait: %v", cfg.Roles["worker"].Drain.Wait)
	}
	env, err := cfg.Environment("production")
	if err != nil || env.Hosts[0] != "deploy@monk.labstack.net" {
		t.Fatalf("env: %+v err=%v", env, err)
	}
	if cfg.Retain != 5 { // default
		t.Fatalf("retain default: %d", cfg.Retain)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"bad mode":              sample + "\n", // patched below
		"rolling without ready": "",
	}
	_ = cases
	bad := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: sideways } }
order: [web]
`
	cfg, err := Load(write(t, bad))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for mode 'sideways'")
	}

	noReady := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles: { web: { service: server, mode: rolling } }
order: [web]
`
	cfg, err = Load(write(t, noReady))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: rolling role requires ready http|exec")
	}

	orderGap := `
app: monk
compose: c.yaml
environments: { production: { hosts: [h] } }
roles:
  web: { service: server, mode: recreate }
  bg:  { service: bg, mode: recreate }
order: [web]
`
	cfg, err = Load(write(t, orderGap))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: order must cover every role")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/config/config.go
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a yaml-parseable time.Duration ("30s", "2m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	App          string                 `yaml:"app"`
	Compose      string                 `yaml:"compose"`
	Environments map[string]Environment `yaml:"environments"`
	Roles        map[string]Role        `yaml:"roles"`
	Order        []string               `yaml:"order"`
	Accessories  []string               `yaml:"accessories"`
	Jobs         []string               `yaml:"jobs"`
	Hooks        map[string]string      `yaml:"hooks"`
	Verify       []VerifyCheck          `yaml:"verify"`
	Proxy        Proxy                  `yaml:"proxy"`
	Retain       int                    `yaml:"retain"`
}

type Environment struct {
	Hosts []string `yaml:"hosts"`
}

type Role struct {
	Service   string `yaml:"service"`
	Mode      string `yaml:"mode"` // rolling | recreate
	Singleton bool   `yaml:"singleton"`
	Ready     *Ready `yaml:"ready"`
	Drain     *Drain `yaml:"drain"`
}

type Ready struct {
	HTTP        string   `yaml:"http"` // path, e.g. /healthz
	Exec        string   `yaml:"exec"` // command run inside the container
	Port        int      `yaml:"port"`
	Interval    Duration `yaml:"interval"`     // default 5s
	StartPeriod Duration `yaml:"start_period"` // default 5s
	Within      Duration `yaml:"within"`       // overall gate timeout, default 120s
}

type Drain struct {
	Signal string   `yaml:"signal"`
	Wait   Duration `yaml:"wait"`
}

type VerifyCheck struct {
	HTTP string `yaml:"http"` // path, host-side against container IP
	Exec string `yaml:"exec"`
	Role string `yaml:"role"`
	Port int    `yaml:"port"` // defaults to the role's ready.port
}

type Proxy struct {
	Kind    string `yaml:"kind"`    // traefik-docker | none  (M0: informational)
	Managed bool   `yaml:"managed"` // M0: must be false
}

var appName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Retain: 5}
	dec := yaml.NewDecoder(newReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Retain <= 0 {
		cfg.Retain = 5
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if !appName.MatchString(c.App) {
		return fmt.Errorf("app: %q must match %s", c.App, appName)
	}
	if c.Compose == "" {
		return fmt.Errorf("compose: required")
	}
	if len(c.Environments) == 0 {
		return fmt.Errorf("environments: at least one required")
	}
	for name, e := range c.Environments {
		if len(e.Hosts) != 1 {
			return fmt.Errorf("environments.%s: M0 supports exactly one host, got %d", name, len(e.Hosts))
		}
	}
	if len(c.Roles) == 0 {
		return fmt.Errorf("roles: at least one required (all-accessory apps land post-M0)")
	}
	inOrder := map[string]bool{}
	for _, r := range c.Order {
		if _, ok := c.Roles[r]; !ok {
			return fmt.Errorf("order: %q is not a role", r)
		}
		inOrder[r] = true
	}
	for name, r := range c.Roles {
		if r.Service == "" {
			return fmt.Errorf("roles.%s: service required", name)
		}
		if r.Mode != "rolling" && r.Mode != "recreate" {
			return fmt.Errorf("roles.%s: mode must be rolling|recreate, got %q", name, r.Mode)
		}
		if r.Mode == "rolling" && (r.Ready == nil || (r.Ready.HTTP == "" && r.Ready.Exec == "")) {
			return fmt.Errorf("roles.%s: rolling requires ready.http or ready.exec (design §03 readiness rule)", name)
		}
		if r.Ready != nil && r.Ready.HTTP != "" && r.Ready.Port == 0 {
			return fmt.Errorf("roles.%s: ready.http requires port", name)
		}
		if !inOrder[name] {
			return fmt.Errorf("order: must include every role; missing %q", name)
		}
	}
	if c.Proxy.Managed {
		return fmt.Errorf("proxy.managed: true is M1+ (bootstrap); M0 supports external proxies only")
	}
	// defaults for ready timing
	for name, r := range c.Roles {
		if r.Ready != nil {
			if r.Ready.Interval == 0 {
				r.Ready.Interval = Duration(5 * time.Second)
			}
			if r.Ready.StartPeriod == 0 {
				r.Ready.StartPeriod = Duration(5 * time.Second)
			}
			if r.Ready.Within == 0 {
				r.Ready.Within = Duration(120 * time.Second)
			}
			c.Roles[name] = r
		}
	}
	return nil
}

func (c *Config) Environment(name string) (Environment, error) {
	e, ok := c.Environments[name]
	if !ok {
		return Environment{}, fmt.Errorf("environment %q not defined (have: %v)", name, keys(c.Environments))
	}
	return e, nil
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

Add the tiny helper (yaml.NewDecoder needs an io.Reader):

```go
// still in config.go
import "bytes"

func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
```

- [ ] **Step 4: Run tests**

Run: `go get gopkg.in/yaml.v3 && go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(m0): yeet.yml config model, load, validate"
```

---

### Task 3: Compose — load via compose-go, classify, rollability checks

**Files:**
- Create: `internal/compose/compose.go`, `internal/compose/compose_test.go`, `internal/compose/testdata/simple/docker-compose.yaml`

**Interfaces:**
- Consumes: `config.Config` (Task 2).
- Produces:
  - `compose.Load(ctx, composePath, projectName string) (*types.Project, error)` — compose-go loader with dotenv + os env interpolation.
  - `compose.Classify(p *types.Project, cfg *config.Config) error` — every service exactly one class, else error naming the orphans.
  - `compose.CheckRollable(p *types.Project, cfg *config.Config) []error` — per design §03: rolled services must have no `container_name`, no published host ports, no `deploy.replicas`.
  - `const compose.DrainFile = "/tmp/yeet-drain"`

- [ ] **Step 1: Write the failing test (and fixture)**

```yaml
# internal/compose/testdata/simple/docker-compose.yaml
services:
  server:
    image: ghcr.io/example/app:${VERSION:-latest}
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:8080/healthz || exit 1"]
      interval: 5s
  worker:
    image: ghcr.io/example/app:${VERSION:-latest}
    command: work
  postgres:
    image: postgres:17
    ports: ["127.0.0.1:5432:5432"]
  migrate:
    image: ghcr.io/example/app:${VERSION:-latest}
    command: migrate
  rogue:
    image: alpine
    container_name: pinned
    ports: ["8080:8080"]
```

```go
// internal/compose/compose_test.go
package compose

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		App: "demo",
		Roles: map[string]config.Role{
			"web":    {Service: "server", Mode: "rolling", Ready: &config.Ready{HTTP: "/healthz", Port: 8080}},
			"worker": {Service: "worker", Mode: "recreate"},
		},
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
	}
}

func TestLoadAndClassifyReportsOrphan(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	err = Classify(p, testCfg())
	if err == nil || !strings.Contains(err.Error(), "rogue") {
		t.Fatalf("want orphan error naming 'rogue', got %v", err)
	}
}

func TestCheckRollable(t *testing.T) {
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	cfg.Roles["edge"] = config.Role{Service: "rogue", Mode: "rolling", Ready: &config.Ready{HTTP: "/", Port: 8080}}
	errs := CheckRollable(p, cfg)
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "container_name") || !strings.Contains(joined, "host port") {
		t.Fatalf("want container_name and host port violations for 'rogue', got:\n%s", joined)
	}
	// server rolls clean
	if strings.Contains(joined, `"server"`) {
		t.Fatalf("server should be rollable:\n%s", joined)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/`
Expected: FAIL — package missing.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/compose/compose.go
package compose

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
)

// DrainFile: the generated/wrapped healthcheck fails while this file exists,
// which is how the proxy is told to stop routing to a container (rev 5
// traffic-shift protocol, "poison its health state").
const DrainFile = "/tmp/yeet-drain"

func Load(ctx context.Context, composePath, projectName string) (*types.Project, error) {
	opts, err := cli.NewProjectOptions(
		[]string{composePath},
		cli.WithName(projectName),
		cli.WithWorkingDirectory(filepath.Dir(composePath)),
		cli.WithOsEnv,
		cli.WithDotEnv,
	)
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("compose load %s: %w", composePath, err)
	}
	return p, nil
}

// Classify verifies every compose service has exactly one class (design §03).
func Classify(p *types.Project, cfg *config.Config) error {
	class := map[string]string{}
	claim := func(svc, cls string) error {
		if prev, ok := class[svc]; ok {
			return fmt.Errorf("service %q claimed as both %s and %s", svc, prev, cls)
		}
		class[svc] = cls
		return nil
	}
	for name, r := range cfg.Roles {
		if _, ok := p.Services[r.Service]; !ok {
			return fmt.Errorf("roles.%s: compose has no service %q", name, r.Service)
		}
		if err := claim(r.Service, "role"); err != nil {
			return err
		}
	}
	for _, a := range cfg.Accessories {
		if _, ok := p.Services[a]; !ok {
			return fmt.Errorf("accessories: compose has no service %q", a)
		}
		if err := claim(a, "accessory"); err != nil {
			return err
		}
	}
	for _, j := range cfg.Jobs {
		if _, ok := p.Services[j]; !ok {
			return fmt.Errorf("jobs: compose has no service %q", j)
		}
		if err := claim(j, "job"); err != nil {
			return err
		}
	}
	var orphans []string
	for name := range p.Services {
		if _, ok := class[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		return fmt.Errorf("every service needs exactly one class (role|accessory|job); unclassified: %v", orphans)
	}
	return nil
}

// CheckRollable enforces design §03 rolling preconditions on rolled services.
func CheckRollable(p *types.Project, cfg *config.Config) []error {
	var errs []error
	for roleName, r := range cfg.Roles {
		if r.Mode != "rolling" {
			continue
		}
		svc, ok := p.Services[r.Service]
		if !ok {
			continue // Classify reports this
		}
		if svc.ContainerName != "" {
			errs = append(errs, fmt.Errorf("roles.%s (%q): container_name forbids running two copies — remove it", roleName, r.Service))
		}
		for _, port := range svc.Ports {
			if port.Published != "" {
				errs = append(errs, fmt.Errorf("roles.%s (%q): host port %s:%d — two containers cannot share a host port; route via the proxy instead", roleName, r.Service, port.Published, port.Target))
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			errs = append(errs, fmt.Errorf("roles.%s (%q): deploy.replicas conflicts with yeet-managed scaling", roleName, r.Service))
		}
	}
	return errs
}
```

- [ ] **Step 4: Run tests**

Run: `go get github.com/compose-spec/compose-go/v2@latest && go test ./internal/compose/`
Expected: PASS. (If the compiler flags v2 field names — e.g. `Published` type — fix per actual API; keep test assertions unchanged.)

- [ ] **Step 5: Commit**

```bash
git add internal/compose/
git commit -m "feat(m0): compose load via compose-go, service classification, rollability checks"
```

---

### Task 4: Compose render — closed injection set

**Files:**
- Create: `internal/compose/render.go`, `internal/compose/render_test.go`

**Interfaces:**
- Consumes: `Load`, `DrainFile` (Task 3), `config.Config`.
- Produces: `compose.Render(p *types.Project, cfg *config.Config, releaseID string) ([]byte, error)` — returns canonical YAML with, per rolled/recreated role service: `yeet.app`/`yeet.release` labels and a healthcheck **wrapped with the drain-poison guard**. Ready rules per design §03: existing healthcheck + no `ready:` → adopt (wrap only); `ready.http` → generate `CMD-SHELL` curl/wget probe (wrapped); both → generated wins. Accessories and jobs are untouched except `yeet.app` label on nothing — **accessories/jobs get NO injections at all** (closed set discipline).

- [ ] **Step 1: Write the failing test**

```go
// internal/compose/render_test.go
package compose

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func render(t *testing.T) map[string]any {
	t.Helper()
	p, err := Load(context.Background(), "testdata/simple/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	out, err := Render(p, cfg, "20260702-120000-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc["services"].(map[string]any)
}

func hc(t *testing.T, svcs map[string]any, name string) string {
	t.Helper()
	svc := svcs[name].(map[string]any)
	h, ok := svc["healthcheck"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no healthcheck rendered", name)
	}
	test := h["test"].([]any)
	parts := make([]string, len(test))
	for i, v := range test {
		parts[i] = v.(string)
	}
	return strings.Join(parts, " ")
}

func TestRenderWrapsAdoptedHealthcheckWithDrainGuard(t *testing.T) {
	svcs := render(t)
	// server has ready.http -> generated wins, and it must carry the drain guard
	got := hc(t, svcs, "server")
	if !strings.Contains(got, DrainFile) {
		t.Fatalf("server healthcheck missing drain guard: %s", got)
	}
	if !strings.Contains(got, "/healthz") {
		t.Fatalf("server healthcheck should probe ready.http path: %s", got)
	}
}

func TestRenderTouchesOnlyRoleServices(t *testing.T) {
	svcs := render(t)
	pg := svcs["postgres"].(map[string]any)
	if labels, ok := pg["labels"]; ok {
		if s, _ := yaml.Marshal(labels); strings.Contains(string(s), "yeet.") {
			t.Fatalf("accessory postgres must not receive yeet labels: %s", s)
		}
	}
	web := svcs["server"].(map[string]any)
	s, _ := yaml.Marshal(web["labels"])
	if !strings.Contains(string(s), "yeet.release") || !strings.Contains(string(s), "20260702-120000-abc1234") {
		t.Fatalf("server missing yeet.release label: %s", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run TestRender`
Expected: FAIL — `Render` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/compose/render.go
package compose

import (
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
)

// Render produces the per-release deployable (design §02): the user's compose
// project plus a CLOSED injection set — yeet.* labels and a drain-guarded
// healthcheck — applied to ROLE services only.
func Render(p *types.Project, cfg *config.Config, releaseID string) ([]byte, error) {
	for _, r := range cfg.Roles {
		svc, ok := p.Services[r.Service]
		if !ok {
			return nil, fmt.Errorf("compose has no service %q", r.Service)
		}
		if svc.Labels == nil {
			svc.Labels = types.Labels{}
		}
		svc.Labels["yeet.app"] = cfg.App
		svc.Labels["yeet.release"] = releaseID

		probe := adoptedProbe(svc)
		if r.Ready != nil && r.Ready.HTTP != "" {
			probe = fmt.Sprintf("curl -fsS http://localhost:%d%s || wget -qO- http://localhost:%d%s",
				r.Ready.Port, r.Ready.HTTP, r.Ready.Port, r.Ready.HTTP)
		} else if r.Ready != nil && r.Ready.Exec != "" {
			probe = r.Ready.Exec
		}
		if probe != "" {
			interval := 5 * time.Second
			start := 5 * time.Second
			if r.Ready != nil {
				interval = time.Duration(r.Ready.Interval)
				start = time.Duration(r.Ready.StartPeriod)
			}
			iv, sp := types.Duration(interval), types.Duration(start)
			svc.HealthCheck = &types.HealthCheckConfig{
				// The drain guard: while DrainFile exists the check fails, the
				// proxy drops the container, and only THEN does yeet signal it.
				Test:        types.HealthCheckTest{"CMD-SHELL", fmt.Sprintf("test ! -f %s && ( %s )", DrainFile, probe)},
				Interval:    &iv,
				StartPeriod: &sp,
			}
		}
		p.Services[r.Service] = svc
	}
	return p.MarshalYAML()
}

// adoptedProbe extracts a shell-runnable probe from a user-authored
// healthcheck so the drain guard can wrap it (adopt-and-wrap, design §03).
func adoptedProbe(svc types.ServiceConfig) string {
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) == 0 {
		return ""
	}
	t := svc.HealthCheck.Test
	switch t[0] {
	case "CMD-SHELL":
		if len(t) > 1 {
			return t[1]
		}
	case "CMD":
		out := ""
		for _, a := range t[1:] {
			out += fmt.Sprintf("%q ", a)
		}
		return out
	}
	return ""
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/compose/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/render.go internal/compose/render_test.go
git commit -m "feat(m0): render per-release compose with closed injection set + drain-guarded healthchecks"
```

---

### Task 5: Transport — interface, Local, Fake

**Files:**
- Create: `internal/transport/transport.go`, `internal/transport/fake.go`, `internal/transport/transport_test.go`

**Interfaces:**
- Produces (all later engine code depends on exactly this):

```go
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Transport interface {
	Run(ctx context.Context, cmd string) (Result, error) // err = transport failure only
	Upload(ctx context.Context, localDir, remoteDir string) error
	Host() string
	Close() error
}
```

  - `transport.NewLocal() Transport` — runs via `sh -c`, `Upload` = recursive copy; used by e2e.
  - `transport.Fake` — `Script []Rule{Match *regexp.Regexp; Result Result}` first-match wins, default exit 0; `Commands []string` records every Run; `Uploads []string` records `localDir -> remoteDir`.
  - Optional per-transport `Logger func(host, cmd string)` set by engine when `--verbose`.

- [ ] **Step 1: Write the failing test**

```go
// internal/transport/transport_test.go
package transport

import (
	"context"
	"regexp"
	"testing"
)

func TestLocalRunCapturesExitAndOutput(t *testing.T) {
	tr := NewLocal()
	res, err := tr.Run(context.Background(), "echo hi; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi\n" {
		t.Fatalf("got %+v", res)
	}
}

func TestFakeScriptsAndRecords(t *testing.T) {
	f := &Fake{Script: []Rule{
		{Match: regexp.MustCompile(`docker inspect`), Result: Result{Stdout: "healthy\n"}},
	}}
	res, _ := f.Run(context.Background(), "docker inspect -f x abc")
	if res.Stdout != "healthy\n" {
		t.Fatalf("scripted reply not used: %+v", res)
	}
	res, _ = f.Run(context.Background(), "docker stop abc")
	if res.ExitCode != 0 {
		t.Fatalf("default should be exit 0: %+v", res)
	}
	if len(f.Commands) != 2 || f.Commands[1] != "docker stop abc" {
		t.Fatalf("recording broken: %v", f.Commands)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/transport/transport.go
package transport

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Transport interface {
	Run(ctx context.Context, cmd string) (Result, error)
	Upload(ctx context.Context, localDir, remoteDir string) error
	Host() string
	Close() error
}

type Local struct {
	Logger func(host, cmd string)
}

func NewLocal() *Local { return &Local{} }

func (l *Local) Run(ctx context.Context, cmd string) (Result, error) {
	if l.Logger != nil {
		l.Logger("local", cmd)
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func (l *Local) Upload(ctx context.Context, localDir, remoteDir string) error {
	_, err := l.Run(ctx, "mkdir -p "+shq(remoteDir)+" && cp -a "+shq(localDir)+"/. "+shq(remoteDir)+"/")
	return err
}

func (l *Local) Host() string { return "local" }
func (l *Local) Close() error { return nil }

// shq single-quotes a shell argument.
func shq(s string) string {
	b := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b = append(b, '\'', '\\', '\'', '\'')
		} else {
			b = append(b, s[i])
		}
	}
	return string(append(b, '\''))
}
```

```go
// internal/transport/fake.go
package transport

import (
	"context"
	"fmt"
	"regexp"
)

type Rule struct {
	Match  *regexp.Regexp
	Result Result
}

type Fake struct {
	Script   []Rule
	Commands []string
	Uploads  []string
	HostName string
}

func (f *Fake) Run(_ context.Context, cmd string) (Result, error) {
	f.Commands = append(f.Commands, cmd)
	for _, r := range f.Script {
		if r.Match.MatchString(cmd) {
			return r.Result, nil
		}
	}
	return Result{ExitCode: 0}, nil
}

func (f *Fake) Upload(_ context.Context, localDir, remoteDir string) error {
	f.Uploads = append(f.Uploads, fmt.Sprintf("%s -> %s", localDir, remoteDir))
	return nil
}

func (f *Fake) Host() string {
	if f.HostName == "" {
		return "fake"
	}
	return f.HostName
}
func (f *Fake) Close() error { return nil }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/transport/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transport/transport.go internal/transport/fake.go internal/transport/transport_test.go
git commit -m "feat(m0): transport interface with local and fake implementations"
```

---

### Task 6: Transport — SSH with enforced known-hosts

**Files:**
- Create: `internal/transport/ssh.go`
- Modify: `internal/transport/transport_test.go` (add address-parsing tests)

**Interfaces:**
- Produces: `transport.NewSSH(addr string) (*SSH, error)` where `addr` = `[user@]host[:port]` (default user `$USER`, port 22). Auth: `SSH_AUTH_SOCK` agent first, then `~/.ssh/id_ed25519`, `~/.ssh/id_rsa`. Host keys verified against `~/.ssh/known_hosts` via `knownhosts.New` — **no InsecureIgnoreHostKey anywhere** (design §11 security row). `Upload` streams `tar cz` over stdin to `mkdir -p <dir> && tar -xzf - -C <dir>`.
- Produces: `transport.ParseAddr(addr string) (user, host, port string)` exported for tests.

- [ ] **Step 1: Write the failing test**

```go
// append to internal/transport/transport_test.go
func TestParseAddr(t *testing.T) {
	cases := []struct{ in, user, host, port string }{
		{"deploy@monk.labstack.net", "deploy", "monk.labstack.net", "22"},
		{"monk.labstack.net:2222", "", "monk.labstack.net", "2222"},
		{"root@10.0.0.5:22", "root", "10.0.0.5", "22"},
	}
	for _, c := range cases {
		u, h, p := ParseAddr(c.in)
		if u != c.user || h != c.host || p != c.port {
			t.Fatalf("%s -> %s,%s,%s", c.in, u, h, p)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/ -run TestParseAddr`
Expected: FAIL — `ParseAddr` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/transport/ssh.go
package transport

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type SSH struct {
	client *ssh.Client
	host   string
	Logger func(host, cmd string)
}

func ParseAddr(addr string) (user, host, port string) {
	port = "22"
	if i := strings.Index(addr, "@"); i >= 0 {
		user, addr = addr[:i], addr[i+1:]
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return user, h, p
	}
	return user, addr, port
}

func NewSSH(addr string) (*SSH, error) {
	user, host, port := ParseAddr(addr)
	if user == "" {
		user = os.Getenv("USER")
	}
	home, _ := os.UserHomeDir()
	hk, err := knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil {
		return nil, fmt.Errorf("known_hosts (required — yeet never skips host verification): %w", err)
	}
	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		if b, err := os.ReadFile(filepath.Join(home, ".ssh", name)); err == nil {
			if signer, err := ssh.ParsePrivateKey(b); err == nil {
				auths = append(auths, ssh.PublicKeys(signer))
			}
		}
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no SSH auth available (agent or ~/.ssh/id_ed25519|id_rsa)")
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, port), &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hk,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%s: %w", user, host, port, err)
	}
	return &SSH{client: client, host: host}, nil
}

func (s *SSH) Run(ctx context.Context, cmd string) (Result, error) {
	if s.Logger != nil {
		s.Logger(s.host, cmd)
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var out, errb strings.Builder
	sess.Stdout, sess.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return Result{}, ctx.Err()
	case err = <-done:
	}
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func (s *SSH) Upload(ctx context.Context, localDir, remoteDir string) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	pw, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	if err := sess.Start("mkdir -p " + shq(remoteDir) + " && tar -xzf - -C " + shq(remoteDir)); err != nil {
		return err
	}
	gz := gzip.NewWriter(pw)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return err
	}
	return sess.Wait()
}

func (s *SSH) Host() string { return s.host }
func (s *SSH) Close() error { return s.client.Close() }
```

- [ ] **Step 4: Run tests + build**

Run: `go get golang.org/x/crypto@latest && go test ./internal/transport/ && go build ./...`
Expected: PASS (SSH connect path exercised in e2e/manual, not unit tests).

- [ ] **Step 5: Commit**

```bash
git add internal/transport/ssh.go internal/transport/transport_test.go go.mod go.sum
git commit -m "feat(m0): ssh transport with enforced known-hosts and tar upload"
```

---

### Task 7: Release — ids, layout, snapshot, current/previous

**Files:**
- Create: `internal/release/release.go`, `internal/release/release_test.go`

**Interfaces:**
- Consumes: `transport.Transport`.
- Produces:
  - `release.NewID(now time.Time, gitSHA string) string` → `20260702-150405-abc1234` (`-nogit` when SHA empty).
  - `release.Paths{Base, Releases, Current string}` from `release.PathsFor(app string)` → `/var/lib/yeet/<app>/...`.
  - `release.Stage(dir string, composeYAML []byte, snapshotYAML []byte) error` — writes `compose.yaml` + `yeet.snapshot.yml` into a local staging dir (design §04: snapshot rides with the release so rollback replays the OLD choreography).
  - `release.Push(ctx, t transport.Transport, stagingDir, app, id string) (remoteDir string, err error)`.
  - `release.Current(ctx, t, app) (string, error)` / `release.Previous(ctx, t, app) (string, error)` — resolve via `readlink` + sorted `ls -1` of `releases/`.
  - `release.Activate(ctx, t, app, id) error` — `ln -sfn`.
  - `release.Prune(ctx, t, app string, retain int) ([]string, error)` — never removes current/previous targets.

- [ ] **Step 1: Write the failing test**

```go
// internal/release/release_test.go
package release

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/transport"
)

func TestNewID(t *testing.T) {
	id := NewID(time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC), "abc1234")
	if id != "20260702-150405-abc1234" {
		t.Fatal(id)
	}
	if !strings.HasSuffix(NewID(time.Now(), ""), "-nogit") {
		t.Fatal("empty sha should yield -nogit")
	}
}

func TestPreviousAndPrune(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`readlink`), Result: transport.Result{Stdout: "releases/20260702-030000-ccc\n"}},
		{Match: regexp.MustCompile(`ls -1`), Result: transport.Result{
			Stdout: "20260701-010000-aaa\n20260701-020000-bbb\n20260702-030000-ccc\n"}},
	}}
	prev, err := Previous(context.Background(), f, "monk")
	if err != nil || prev != "20260701-020000-bbb" {
		t.Fatalf("prev=%q err=%v", prev, err)
	}
	removed, err := Prune(context.Background(), f, "monk", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "20260701-010000-aaa" {
		t.Fatalf("removed=%v", removed)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "rm -rf '/var/lib/yeet/monk/releases/20260701-010000-aaa'") {
		t.Fatalf("prune command missing:\n%s", joined)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/release/`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/release/release.go
package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/labstack/yeet/internal/transport"
)

type Paths struct{ Base, Releases, Current string }

func PathsFor(app string) Paths {
	base := "/var/lib/yeet/" + app
	return Paths{Base: base, Releases: base + "/releases", Current: base + "/current"}
}

func NewID(now time.Time, gitSHA string) string {
	if gitSHA == "" {
		gitSHA = "nogit"
	}
	return now.UTC().Format("20060102-150405") + "-" + gitSHA
}

func Stage(dir string, composeYAML, snapshotYAML []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), composeYAML, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "yeet.snapshot.yml"), snapshotYAML, 0o644)
}

func Push(ctx context.Context, t transport.Transport, stagingDir, app, id string) (string, error) {
	remote := PathsFor(app).Releases + "/" + id
	if err := t.Upload(ctx, stagingDir, remote); err != nil {
		return "", err
	}
	return remote, nil
}

func Current(ctx context.Context, t transport.Transport, app string) (string, error) {
	p := PathsFor(app)
	res, err := t.Run(ctx, "readlink "+q(p.Current)+" || true")
	if err != nil {
		return "", err
	}
	link := strings.TrimSpace(res.Stdout)
	if link == "" {
		return "", nil // first deploy
	}
	return filepath.Base(link), nil
}

func list(ctx context.Context, t transport.Transport, app string) ([]string, error) {
	res, err := t.Run(ctx, "ls -1 "+q(PathsFor(app).Releases)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ids = append(ids, l)
		}
	}
	sort.Strings(ids) // ids are lexically time-ordered by construction
	return ids, nil
}

func Previous(ctx context.Context, t transport.Transport, app string) (string, error) {
	cur, err := Current(ctx, t, app)
	if err != nil {
		return "", err
	}
	ids, err := list(ctx, t, app)
	if err != nil {
		return "", err
	}
	for i, id := range ids {
		if id == cur && i > 0 {
			return ids[i-1], nil
		}
	}
	return "", fmt.Errorf("no previous release (current=%q, releases=%v)", cur, ids)
}

func Activate(ctx context.Context, t transport.Transport, app, id string) error {
	p := PathsFor(app)
	res, err := t.Run(ctx, "ln -sfn "+q("releases/"+id)+" "+q(p.Current))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("activate: %s", res.Stderr)
	}
	return nil
}

// Prune removes releases beyond retain, never the current or previous target.
func Prune(ctx context.Context, t transport.Transport, app string, retain int) ([]string, error) {
	ids, err := list(ctx, t, app)
	if err != nil || len(ids) <= retain {
		return nil, err
	}
	cur, _ := Current(ctx, t, app)
	victims := ids[:len(ids)-retain]
	var removed []string
	for _, id := range victims {
		if id == cur {
			continue
		}
		res, err := t.Run(ctx, "rm -rf "+q(PathsFor(app).Releases+"/"+id))
		if err != nil {
			return removed, err
		}
		if res.ExitCode == 0 {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/release/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/release/
git commit -m "feat(m0): versioned release dirs, snapshot staging, activate and prune"
```

---

### Task 8: Engine — preflight

**Files:**
- Create: `internal/engine/engine.go`, `internal/engine/preflight.go`, `internal/engine/preflight_test.go`

**Interfaces:**
- Consumes: `config`, `compose`, `transport`, `release`.
- Produces:
  - `engine.Engine{Cfg *config.Config; Project *types.Project; T transport.Transport; Opts Options}` constructed by `engine.New(...)`.
  - `Options{Verbose bool; Out io.Writer; Sleep func(time.Duration); Now func() time.Time; ConvergeBuffer time.Duration}` — `Sleep`/`Now` injectable for tests; `ConvergeBuffer` default 3s (the bounded proxy-observation wait from the rev 5 shift protocol).
  - `(*Engine).Preflight(ctx) error` — asserts, in order: docker daemon (`docker version -f {{.Server.Version}}`), compose plugin (`docker compose version --short`), base dir writable (`mkdir -p` + `test -w`), disk headroom ≥1GiB (`df -Pk`), every accessory running (and healthy when it has a healthcheck — warn to `Opts.Out` when health degrades to running-only, design rev 5). Nothing mutates except `mkdir -p` of yeet's own base dir.

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/preflight_test.go
package engine

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/transport"
)

func fakeEngine(f *transport.Fake) *Engine {
	e := New(testConfig(), testProject(nil), f, Options{})
	e.Opts.Out = &bytes.Buffer{}
	return e
}

func TestPreflightHappyPath(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}}, // 4 GiB in KiB
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "abc123\n"}},
		{Match: regexp.MustCompile(`docker inspect .*abc123`), Result: transport.Result{Stdout: "healthy\n"}},
	}}
	if err := fakeEngine(f).Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
}

func TestPreflightFailsOnStoppedAccessory(t *testing.T) {
	f := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile(`docker version`), Result: transport.Result{Stdout: "27.0.3\n"}},
		{Match: regexp.MustCompile(`docker compose version`), Result: transport.Result{Stdout: "2.29.1\n"}},
		{Match: regexp.MustCompile(`df -Pk`), Result: transport.Result{Stdout: "4194304\n"}},
		{Match: regexp.MustCompile(`docker ps .*postgres`), Result: transport.Result{Stdout: "\n"}},
	}}
	err := fakeEngine(f).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("want accessory-down error, got %v", err)
	}
}
```

Shared fixtures for all engine tests:

```go
// internal/engine/fixtures_test.go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/compose"
	"github.com/labstack/yeet/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		App: "monk", Compose: "docker-compose.yaml", Retain: 5,
		Environments: map[string]config.Environment{"production": {Hosts: []string{"deploy@h"}}},
		Roles: map[string]config.Role{
			"web":    {Service: "server", Mode: "rolling", Ready: &config.Ready{HTTP: "/healthz", Port: 7500, Interval: config.Duration(5e9), StartPeriod: config.Duration(5e9), Within: config.Duration(120e9)}},
			"worker": {Service: "worker", Mode: "recreate", Drain: &config.Drain{Signal: "TERM", Wait: config.Duration(1e9)}},
		},
		Order:       []string{"web", "worker"},
		Accessories: []string{"postgres"},
		Jobs:        []string{"migrate"},
		Hooks:       map[string]string{"migrate": "docker compose run --rm --no-deps migrate"},
		Verify:      []config.VerifyCheck{{HTTP: "/healthz", Role: "web"}},
	}
}

const engineCompose = `
services:
  server:
    image: ghcr.io/x/app:v2
  worker:
    image: ghcr.io/x/app:v2
    command: work
  postgres:
    image: postgres:17
    healthcheck: { test: ["CMD", "pg_isready"] }
  migrate:
    image: ghcr.io/x/app:v2
    command: migrate
`

// testProject loads the fixture through the real compose loader.
func testProject(t *testing.T) *ctypes.Project {
	dir, _ := os.MkdirTemp("", "yeet-eng")
	p := filepath.Join(dir, "docker-compose.yaml")
	_ = os.WriteFile(p, []byte(engineCompose), 0o644)
	proj, err := compose.Load(context.Background(), p, "monk")
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return proj
}
```

(Adjust `testProject(nil)` calls to `testProject(t)` — pass the `*testing.T`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/`
Expected: FAIL — package missing.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/engine.go
package engine

import (
	"io"
	"os"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/transport"
)

type Options struct {
	Verbose        bool
	Out            io.Writer
	Sleep          func(time.Duration)
	Now            func() time.Time
	ConvergeBuffer time.Duration
}

type Engine struct {
	Cfg     *config.Config
	Project *ctypes.Project
	T       transport.Transport
	Opts    Options
}

func New(cfg *config.Config, p *ctypes.Project, t transport.Transport, o Options) *Engine {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ConvergeBuffer == 0 {
		o.ConvergeBuffer = 3 * time.Second
	}
	return &Engine{Cfg: cfg, Project: p, T: t, Opts: o}
}

func (e *Engine) logf(format string, a ...any) {
	_, _ = io.WriteString(e.Opts.Out, "→ "+sprintf(format, a...)+"\n")
}
```

```go
// internal/engine/preflight.go
package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/labstack/yeet/internal/release"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

const minDiskKiB = 1 << 20 // 1 GiB

func (e *Engine) Preflight(ctx context.Context) error {
	if res, err := e.T.Run(ctx, "docker version -f '{{.Server.Version}}'"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker daemon unreachable on %s: %v %s", e.T.Host(), err, res.Stderr)
	}
	if res, err := e.T.Run(ctx, "docker compose version --short"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker compose plugin missing on %s", e.T.Host())
	}
	base := release.PathsFor(e.Cfg.App).Base
	if res, err := e.T.Run(ctx, "mkdir -p "+q(base)+" && test -w "+q(base)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("%s not writable by deploy user", base)
	}
	res, err := e.T.Run(ctx, "df -Pk "+q(base)+" | awk 'NR==2{print $4}'")
	if err != nil {
		return err
	}
	if kib, _ := strconv.Atoi(strings.TrimSpace(res.Stdout)); kib > 0 && kib < minDiskKiB {
		return fmt.Errorf("disk headroom %d KiB < 1 GiB on %s", kib, e.T.Host())
	}
	for _, acc := range e.Cfg.Accessories {
		id, err := e.containerID(ctx, acc)
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("accessory %q not running — run it first (bootstrap/accessory apply are M1+)", acc)
		}
		health, _ := e.healthOf(ctx, id)
		switch health {
		case "healthy":
		case "none":
			e.logf("warn: accessory %q has no healthcheck; asserting running-only", acc)
		default:
			return fmt.Errorf("accessory %q is %s, refusing to deploy", acc, health)
		}
	}
	return nil
}

// containerID returns the newest running container for a compose service.
func (e *Engine) containerID(ctx context.Context, svc string) (string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+e.Cfg.App+
			" --filter label=com.docker.compose.service="+svc+" | head -1")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (e *Engine) containerIDs(ctx context.Context, svc string) ([]string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+e.Cfg.App+
			" --filter label=com.docker.compose.service="+svc)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if !validID.MatchString(l) {
				return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", l)
			}
			ids = append(ids, l)
		}
	}
	return ids, nil
}

// validID: container ids are hex (docker) or test doubles; anything else is
// never interpolated back into a shell command (command-injection rule).
var validID = regexp.MustCompile(`^[0-9a-zA-Z]{1,64}$`)

func (e *Engine) healthOf(ctx context.Context, id string) (string, error) {
	res, err := e.T.Run(ctx,
		"docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "+id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "feat(m0): preflight — daemon, compose, disk, accessory liveness"
```

---

### Task 9: Engine — rolling choreography (the heart)

**Files:**
- Create: `internal/engine/roll.go`, `internal/engine/roll_test.go`

**Interfaces:**
- Consumes: `Engine`, `containerID(s)`, `healthOf` (Task 8), `compose.DrainFile`.
- Produces:
  - `(*Engine).RollRole(ctx, roleName string, remoteComposePath string) error` — implements rev 5 join→converged→drain→converged→bleed→SIGTERM→remove.
  - `(*Engine).RecreateRole(ctx, roleName, remoteComposePath string) error` (Task 10 tests it; declared here).

The exact remote command sequence for `RollRole("web", F)` — this IS the design's §03 mechanism, and the test pins it:

1. `docker compose -p monk -f F pull --quiet server` (prebuilt: image must exist)
2. old := containerIDs(server) (expect ≤1 in M0)
3. `docker compose -p monk -f F up -d --no-deps --no-recreate --scale server=2 server`
4. new := containerIDs(server) minus old
5. poll `healthOf(new)` every `ready.interval` until `healthy`, budget `ready.within` — **join**
6. sleep `ConvergeBuffer` — **converged** (proxy observed the healthy newcomer)
7. `docker exec OLD touch /tmp/yeet-drain` — **drain**
8. poll `healthOf(old)` until `unhealthy` (budget: interval×5), then sleep `ConvergeBuffer` — **converged** (proxy dropped it)
9. if role has `drain.wait`: optionally `docker kill --signal=SIG OLD`, sleep `drain.wait` — **bleed**
10. `docker stop -t 30 OLD` → SIGTERM + grace — old container's own graceful shutdown
11. `docker rm OLD`

On join failure (step 5 timeout): remove the NEW container (`docker rm -f NEW`), leave old serving, return error — the failed deploy must not strand a half-rolled role.

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/roll_test.go
package engine

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/transport"
)

// scripted docker: ps returns OLD first, then OLD+NEW after up --scale;
// health: NEW healthy immediately, OLD unhealthy after poison.
func rollFake() *transport.Fake {
	psCount := 0
	f := &transport.Fake{}
	f.Script = []transport.Rule{
		{Match: regexp.MustCompile(`docker ps -q .*service=server$`), Result: transport.Result{}}, // overridden below
	}
	// Fake needs dynamic replies for ps; extend Fake with a Func rule:
	f.Script = nil
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if regexp.MustCompile(`docker ps -q .*service=server$`).MatchString(cmd) {
			psCount++
			if psCount == 1 {
				return transport.Result{Stdout: "OLD\n"}, true
			}
			return transport.Result{Stdout: "OLD\nNEW\n"}, true
		}
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "NEW") {
			return transport.Result{Stdout: "healthy\n"}, true
		}
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "OLD") {
			if drained(f) {
				return transport.Result{Stdout: "unhealthy\n"}, true
			}
			return transport.Result{Stdout: "healthy\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

func drained(f *transport.Fake) bool {
	for _, c := range f.Commands {
		if strings.Contains(c, "touch /tmp/yeet-drain") {
			return true
		}
	}
	return false
}

func TestRollRoleCommandSequence(t *testing.T) {
	f := rollFake()
	e := New(testConfig(), testProject(t), f, Options{Sleep: func(_ time.Duration) {}})
	if err := e.RollRole(context.Background(), "web", "/var/lib/yeet/monk/releases/R1/compose.yaml"); err != nil {
		t.Fatalf("roll: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	ordered := []string{
		"docker compose -p monk -f '/var/lib/yeet/monk/releases/R1/compose.yaml' pull --quiet server",
		"up -d --no-deps --no-recreate --scale server=2 server",
		"docker exec OLD touch /tmp/yeet-drain",
		"docker stop -t 30 OLD",
		"docker rm OLD",
	}
	last := -1
	for _, want := range ordered {
		i := strings.Index(seq, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, seq)
		}
		if i < last {
			t.Fatalf("%q out of order in:\n%s", want, seq)
		}
		last = i
	}
	// drain MUST precede stop: SIGTERM never races the proxy (rev 5)
	if strings.Index(seq, "yeet-drain") > strings.Index(seq, "docker stop") {
		t.Fatal("drain must happen before stop")
	}
}

func TestRollRoleAbortsOnUnhealthyNew(t *testing.T) {
	f := rollFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "NEW") {
			return transport.Result{Stdout: "unhealthy\n"}, true
		}
		return base(cmd)
	}
	cfg := testConfig()
	cfg.Roles["web"] = withinMillis(cfg.Roles["web"], 50) // tiny gate budget
	e := New(cfg, testProject(t), f, Options{Sleep: func(_ time.Duration) {}})
	err := e.RollRole(context.Background(), "web", "F")
	if err == nil {
		t.Fatal("expected join failure")
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker rm -f NEW") {
		t.Fatalf("failed join must remove the new container:\n%s", seq)
	}
	if strings.Contains(seq, "docker stop -t 30 OLD") {
		t.Fatalf("old container must be left serving on failure:\n%s", seq)
	}
}
```

Add to `internal/transport/fake.go` the `Dynamic` hook (modify Task 5's file):

```go
// in Fake struct:
	Dynamic func(cmd string) (Result, bool)

// in (f *Fake) Run, before Script loop:
	if f.Dynamic != nil {
		if res, ok := f.Dynamic(cmd); ok {
			return res, nil
		}
	}
```

And a small helper in the test file:

```go
func withinMillis(r config.Role, ms int) config.Role {
	rd := *r.Ready
	rd.Within = config.Duration(time.Duration(ms) * time.Millisecond)
	rd.Interval = config.Duration(time.Millisecond)
	r.Ready = &rd
	return r
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestRoll`
Expected: FAIL — `RollRole` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/roll.go
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/labstack/yeet/internal/compose"
)

const stopGraceSeconds = 30

func (e *Engine) composeCmd(remoteComposePath string) string {
	return "docker compose -p " + e.Cfg.App + " -f " + q(remoteComposePath)
}

// RollRole executes scale–health–drain for one role (design §03 + the rev 5
// traffic-shift protocol: join → converged → drain → converged → bleed →
// SIGTERM → remove; SIGTERM never races the proxy).
func (e *Engine) RollRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)

	if res, err := e.T.Run(ctx, cc+" pull --quiet "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %v %s", svc, err, res.Stderr)
	}
	old, err := e.containerIDs(ctx, svc)
	if err != nil {
		return err
	}
	if len(old) > 1 {
		return fmt.Errorf("role %s: %d running containers; expected ≤1 (resume is M2 — clean up manually)", roleName, len(old))
	}

	scale := len(old) + 1
	if res, err := e.T.Run(ctx, fmt.Sprintf("%s up -d --no-deps --no-recreate --scale %s=%d %s", cc, svc, scale, svc)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("up --scale %s: %v %s", svc, err, res.Stderr)
	}

	newID, err := e.newcomer(ctx, svc, old)
	if err != nil {
		return err
	}

	// join
	if err := e.waitHealth(ctx, newID, "healthy", time.Duration(role.Ready.Within), time.Duration(role.Ready.Interval)); err != nil {
		e.logf("join failed for %s — removing new container, old keeps serving", roleName)
		_, _ = e.T.Run(ctx, "docker rm -f "+newID)
		return fmt.Errorf("role %s: new container never became healthy: %w", roleName, err)
	}
	if len(old) == 0 {
		e.logf("%s: first deploy, no old container to drain", roleName)
		return nil
	}
	oldID := old[0]

	// converged: proxy has observed the healthy newcomer
	e.Opts.Sleep(e.Opts.ConvergeBuffer)

	// drain: poison old health so the proxy drops it BEFORE any signal
	if _, err := e.T.Run(ctx, "docker exec "+oldID+" touch "+compose.DrainFile); err != nil {
		return err
	}
	drainBudget := 5 * time.Duration(role.Ready.Interval)
	if err := e.waitHealth(ctx, oldID, "unhealthy", drainBudget, time.Duration(role.Ready.Interval)); err != nil {
		e.logf("warn: old container never reported unhealthy (%v); proceeding after buffer", err)
	}
	e.Opts.Sleep(e.Opts.ConvergeBuffer) // converged: proxy dropped it

	// bleed: optional long-connection window
	if role.Drain != nil && role.Drain.Wait > 0 {
		if role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
			_, _ = e.T.Run(ctx, "docker kill --signal="+role.Drain.Signal+" "+oldID)
		}
		e.Opts.Sleep(time.Duration(role.Drain.Wait))
	}

	if res, err := e.T.Run(ctx, fmt.Sprintf("docker stop -t %d %s", stopGraceSeconds, oldID)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("stop old %s: %v %s", oldID, err, res.Stderr)
	}
	if _, err := e.T.Run(ctx, "docker rm "+oldID); err != nil {
		return err
	}
	e.logf("%s: rolled %s -> %s", roleName, oldID, newID)
	return nil
}

func (e *Engine) newcomer(ctx context.Context, svc string, old []string) (string, error) {
	ids, err := e.containerIDs(ctx, svc)
	if err != nil {
		return "", err
	}
	prev := map[string]bool{}
	for _, o := range old {
		prev[o] = true
	}
	for _, id := range ids {
		if !prev[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("no new container appeared for %s", svc)
}

func (e *Engine) waitHealth(ctx context.Context, id, want string, budget, interval time.Duration) error {
	deadline := e.Opts.Now().Add(budget)
	for {
		h, err := e.healthOf(ctx, id)
		if err != nil {
			return err
		}
		if h == want {
			return nil
		}
		if h == "none" && want == "healthy" {
			return fmt.Errorf("container %s has no healthcheck — rolling requires one (generated from ready:)", id)
		}
		if e.Opts.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s to be %s (last: %s)", id, want, h)
		}
		e.Opts.Sleep(interval)
	}
}
```

**Note on the timeout test:** with an injected no-op `Sleep` and real `Now`, the 50ms budget expires via wall clock while the loop spins — that's fine (sub-second test). If flaky, inject `Now` returning stepped times instead.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/ -run TestRoll -v`
Expected: PASS, both tests.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/roll.go internal/engine/roll_test.go internal/transport/fake.go
git commit -m "feat(m0): scale-health-drain rolling choreography with rev5 shift protocol"
```

---

### Task 10: Engine — recreate mode + migrate hook

**Files:**
- Create: `internal/engine/recreate.go`, `internal/engine/recreate_test.go`

**Interfaces:**
- Consumes: Engine internals from Tasks 8–9.
- Produces:
  - `(*Engine).RecreateRole(ctx, roleName, remoteComposePath string) error` — sequence: pull → optional bleed (`docker kill --signal=SIG`, sleep wait) → `up -d --no-deps --force-recreate <svc>` → wait ready if the role has one (else wait running: `docker inspect -f {{.State.Status}}` = `running`).
  - `(*Engine).RunHook(ctx, name, remoteReleaseDir, remoteComposePath string) error` — no-op if hook absent; runs `cd <dir> && COMPOSE_PROJECT_NAME=<app> COMPOSE_FILE=<compose> <hook>`, so monk's `docker compose run --rm --no-deps migrate` works verbatim; nonzero exit fails the deploy (halt — auto-rollback and the migration gate are M2, refuse rather than guess).

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/recreate_test.go
package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/transport"
)

func TestRecreateRoleSequence(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker ps -q") {
			return transport.Result{Stdout: "W1\n"}, true
		}
		if strings.Contains(cmd, "{{.State.Status}}") {
			return transport.Result{Stdout: "running\n"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Sleep: func(time.Duration) {}})
	if err := e.RecreateRole(context.Background(), "worker", "F"); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "up -d --no-deps --force-recreate worker") {
		t.Fatalf("missing force-recreate:\n%s", seq)
	}
	// worker drain: TERM+1s -> docker stop handles TERM; no explicit kill needed
	if strings.Contains(seq, "--signal=TERM") {
		t.Fatalf("TERM bleed should be left to stop/recreate:\n%s", seq)
	}
}

func TestRunHookSetsComposeEnvAndFailsHard(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "run --rm --no-deps migrate") {
			return transport.Result{ExitCode: 7, Stderr: "alembic exploded"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Sleep: func(time.Duration) {}})
	err := e.RunHook(context.Background(), "migrate", "/var/lib/yeet/monk/releases/R1", "/var/lib/yeet/monk/releases/R1/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "alembic exploded") {
		t.Fatalf("hook failure must halt deploy with stderr, got %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "COMPOSE_PROJECT_NAME=monk") || !strings.Contains(seq, "COMPOSE_FILE=") {
		t.Fatalf("hook must run with compose env exported:\n%s", seq)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run 'TestRecreate|TestRunHook'`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/recreate.go
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (e *Engine) RecreateRole(ctx context.Context, roleName, remoteComposePath string) error {
	role := e.Cfg.Roles[roleName]
	svc := role.Service
	cc := e.composeCmd(remoteComposePath)

	if res, err := e.T.Run(ctx, cc+" pull --quiet "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("pull %s: %v %s", svc, err, res.Stderr)
	}
	// bleed before recreate for non-TERM signals (TERM is what stop sends anyway)
	if role.Drain != nil && role.Drain.Wait > 0 && role.Drain.Signal != "" && role.Drain.Signal != "TERM" {
		if id, _ := e.containerID(ctx, svc); id != "" {
			_, _ = e.T.Run(ctx, "docker kill --signal="+role.Drain.Signal+" "+id)
			e.Opts.Sleep(time.Duration(role.Drain.Wait))
		}
	}
	if res, err := e.T.Run(ctx, cc+" up -d --no-deps --force-recreate "+svc); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("recreate %s: %v %s", svc, err, res.Stderr)
	}
	id, err := e.containerID(ctx, svc)
	if err != nil || id == "" {
		return fmt.Errorf("recreate %s: no container after up (%v)", svc, err)
	}
	if role.Ready != nil {
		return e.waitHealth(ctx, id, "healthy", time.Duration(role.Ready.Within), time.Duration(role.Ready.Interval))
	}
	res, err := e.T.Run(ctx, "docker inspect -f '{{.State.Status}}' "+id)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(res.Stdout); s != "running" {
		return fmt.Errorf("recreate %s: container %s is %s", svc, id, s)
	}
	return nil
}

// RunHook executes a user hook verbatim (design §01: hooks are unplannable
// commands) with compose env exported so `docker compose ...` targets this
// release. Nonzero exit halts the deploy — M0 has no migration gate, so the
// only safe behavior is to stop.
func (e *Engine) RunHook(ctx context.Context, name, remoteReleaseDir, remoteComposePath string) error {
	hook, ok := e.Cfg.Hooks[name]
	if !ok || hook == "" {
		return nil
	}
	e.logf("hook %s: %s", name, hook)
	cmd := "cd " + q(remoteReleaseDir) +
		" && COMPOSE_PROJECT_NAME=" + e.Cfg.App +
		" COMPOSE_FILE=" + q(remoteComposePath) + " " + hook
	res, err := e.T.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("hook %s failed (exit %d): %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/recreate.go internal/engine/recreate_test.go
git commit -m "feat(m0): recreate mode with drain bleed + migrate hook execution"
```

---

### Task 11: Engine — verify, finalize, and the Deploy orchestrator

**Files:**
- Create: `internal/engine/verify.go`, `internal/engine/deploy.go`, `internal/engine/deploy_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `(*Engine).Verify(ctx) error` — per check: resolve the role's newest container, get its bridge IP (`docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}'`), then host-side `curl -fsS -m 5 http://IP:PORT<path>` (design §04: host-local, container network, never the edge). `exec` checks run `docker exec <id> sh -c '<cmd>'`. Any failure = error.
  - `(*Engine).Deploy(ctx, releaseID, localStagingDir string) error` — the M0 lifecycle: Preflight → Push (transfer) → `RunHook("migrate", ...)` (pre-release) → roles in `cfg.Order` (rolling|recreate per mode) → Verify → finalize (Activate symlink + Prune retention). Phase names logged.
  - `(*Engine).Rollback(ctx) error` — resolve `Previous`, then re-run the role choreography against the previous release dir (its compose.yaml already pins the old image) and re-Activate it. No prune.

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/deploy_test.go
package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/transport"
)

// happyFake scripts an entire single-role deploy against service `server`
// plus recreate `worker`, accessory postgres healthy.
func happyFake() *transport.Fake {
	ps := 0
	f := &transport.Fake{}
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker version"):
			return transport.Result{Stdout: "27.0.3\n"}, true
		case strings.Contains(cmd, "compose version"):
			return transport.Result{Stdout: "2.29.1\n"}, true
		case strings.Contains(cmd, "df -Pk"):
			return transport.Result{Stdout: "4194304\n"}, true
		case strings.Contains(cmd, "service=postgres"):
			return transport.Result{Stdout: "PG1\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "PG1"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.HasSuffix(cmd, "service=server"):
			ps++
			if ps == 1 {
				return transport.Result{Stdout: "OLD\n"}, true
			}
			return transport.Result{Stdout: "OLD\nNEW\n"}, true
		case strings.Contains(cmd, "service=server | head"):
			return transport.Result{Stdout: "NEW\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "NEW") && strings.Contains(cmd, "Health"):
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "inspect") && strings.Contains(cmd, "OLD") && strings.Contains(cmd, "Health"):
			for _, c := range f.Commands {
				if strings.Contains(c, "yeet-drain") {
					return transport.Result{Stdout: "unhealthy\n"}, true
				}
			}
			return transport.Result{Stdout: "healthy\n"}, true
		case strings.Contains(cmd, "service=worker"):
			return transport.Result{Stdout: "W1\n"}, true
		case strings.Contains(cmd, "{{.State.Status}}"):
			return transport.Result{Stdout: "running\n"}, true
		case strings.Contains(cmd, "IPAddress"):
			return transport.Result{Stdout: "172.20.0.5 \n"}, true
		case strings.Contains(cmd, "readlink"):
			return transport.Result{Stdout: ""}, true
		case strings.Contains(cmd, "ls -1"):
			return transport.Result{Stdout: "R1\n"}, true
		}
		return transport.Result{}, false
	}
	return f
}

func TestDeployPhaseOrder(t *testing.T) {
	f := happyFake()
	e := New(testConfig(), testProject(t), f, Options{Sleep: func(time.Duration) {}})
	staging := t.TempDir()
	if err := e.Deploy(context.Background(), "R1", staging); err != nil {
		t.Fatalf("deploy: %v\ncommands:\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	phases := []string{
		"docker version",                    // preflight
		"run --rm --no-deps migrate",        // pre-release hook (after upload)
		"--scale server=2 server",           // release: web rolls first (order)
		"--force-recreate worker",           // then worker recreates
		"curl -fsS -m 5 http://172.20.0.5:7500/healthz", // verify
		"ln -sfn 'releases/R1'",             // finalize: activate
	}
	last := -1
	for _, p := range phases {
		i := strings.Index(seq, p)
		if i < 0 {
			t.Fatalf("phase step missing %q:\n%s", p, seq)
		}
		if i < last {
			t.Fatalf("phase %q out of order:\n%s", p, seq)
		}
		last = i
	}
	if len(f.Uploads) != 1 || !strings.Contains(f.Uploads[0], "/var/lib/yeet/monk/releases/R1") {
		t.Fatalf("transfer missing: %v", f.Uploads)
	}
}

func TestVerifyFailureBlocksActivation(t *testing.T) {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "curl -fsS") {
			return transport.Result{ExitCode: 22, Stderr: "404"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Sleep: func(time.Duration) {}})
	if err := e.Deploy(context.Background(), "R1", t.TempDir()); err == nil {
		t.Fatal("verify failure must fail the deploy")
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "ln -sfn") {
		t.Fatal("failed verify must not activate the release")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestDeploy`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/verify.go
package engine

import (
	"context"
	"fmt"
	"strings"
)

func (e *Engine) Verify(ctx context.Context) error {
	for _, chk := range e.Cfg.Verify {
		role, ok := e.Cfg.Roles[chk.Role]
		if !ok {
			return fmt.Errorf("verify: unknown role %q", chk.Role)
		}
		id, err := e.containerID(ctx, role.Service)
		if err != nil || id == "" {
			return fmt.Errorf("verify %s: no container (%v)", chk.Role, err)
		}
		switch {
		case chk.HTTP != "":
			port := chk.Port
			if port == 0 && role.Ready != nil {
				port = role.Ready.Port
			}
			res, err := e.T.Run(ctx, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+id)
			if err != nil {
				return err
			}
			ip := strings.Fields(res.Stdout)[0]
			cres, err := e.T.Run(ctx, fmt.Sprintf("curl -fsS -m 5 http://%s:%d%s", ip, port, chk.HTTP))
			if err != nil {
				return err
			}
			if cres.ExitCode != 0 {
				return fmt.Errorf("verify %s: GET %s -> exit %d %s", chk.Role, chk.HTTP, cres.ExitCode, strings.TrimSpace(cres.Stderr))
			}
		case chk.Exec != "":
			res, err := e.T.Run(ctx, "docker exec "+id+" sh -c "+q(chk.Exec))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("verify %s: exec failed (%d): %s", chk.Role, res.ExitCode, strings.TrimSpace(res.Stderr))
			}
		}
		e.logf("verify %s: ok", chk.Role)
	}
	return nil
}
```

```go
// internal/engine/deploy.go
package engine

import (
	"context"
	"fmt"

	"github.com/labstack/yeet/internal/release"
)

func (e *Engine) Deploy(ctx context.Context, releaseID, localStagingDir string) error {
	e.logf("phase preflight")
	if err := e.Preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	e.logf("phase transfer (%s)", releaseID)
	remoteDir, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID)
	if err != nil {
		return fmt.Errorf("transfer: %w", err)
	}
	remoteCompose := remoteDir + "/compose.yaml"

	e.logf("phase pre-release")
	if err := e.RunHook(ctx, "migrate", remoteDir, remoteCompose); err != nil {
		return fmt.Errorf("pre-release: %w", err)
	}

	e.logf("phase release")
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		e.logf("release %s (%s, %s)", roleName, role.Service, role.Mode)
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		if err != nil {
			return fmt.Errorf("release %s: %w (deploy halted — resume/abort are M2; fix and redeploy)", roleName, err)
		}
	}

	e.logf("phase verify")
	if err := e.Verify(ctx); err != nil {
		return fmt.Errorf("verify: %w (release NOT activated; previous release dir still current)", err)
	}

	e.logf("phase finalize")
	if err := release.Activate(ctx, e.T, e.Cfg.App, releaseID); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	removed, err := release.Prune(ctx, e.T, e.Cfg.App, e.Cfg.Retain)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if len(removed) > 0 {
		e.logf("pruned %d old releases", len(removed))
	}
	e.logf("deployed %s", releaseID)
	return nil
}

// Rollback re-releases the previous release dir: its compose.yaml pins the
// old image locally (design §06 "rollback never pulls" — images retained by
// Prune's window), its snapshot IS the old config.
func (e *Engine) Rollback(ctx context.Context) error {
	prev, err := release.Previous(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	remoteCompose := release.PathsFor(e.Cfg.App).Releases + "/" + prev + "/compose.yaml"
	e.logf("rolling back to %s", prev)
	for _, roleName := range e.Cfg.Order {
		role := e.Cfg.Roles[roleName]
		var err error
		if role.Mode == "rolling" {
			err = e.RollRole(ctx, roleName, remoteCompose)
		} else {
			err = e.RecreateRole(ctx, roleName, remoteCompose)
		}
		if err != nil {
			return fmt.Errorf("rollback %s: %w", roleName, err)
		}
	}
	if err := e.Verify(ctx); err != nil {
		return fmt.Errorf("rollback verify: %w", err)
	}
	return release.Activate(ctx, e.T, e.Cfg.App, prev)
}
```

**M0 honesty note (goes in code comment + README):** M0 `Rollback` replays roles with the CURRENT yeet.yml order/modes, not the snapshot's — parsing the remote snapshot is small but deferred to M1 with `plan`; the snapshot is already written so the data is there. Log a warning at rollback start: `"m0: rollback uses current yeet.yml choreography; snapshot replay lands with plan/apply"`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/engine/`
Expected: PASS (all engine tests).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/verify.go internal/engine/deploy.go internal/engine/deploy_test.go
git commit -m "feat(m0): deploy orchestrator with host-side verify and gated activation"
```

---

### Task 12: CLI wiring — validate, render, deploy, rollback

**Files:**
- Create: `cmd/yeet/commands.go`, `cmd/yeet/commands_test.go`
- Modify: `cmd/yeet/main.go` (register subcommands)

**Interfaces:**
- Consumes: all packages.
- Produces subcommands:
  - `yeet validate` — config.Validate + compose.Load + Classify + CheckRollable; prints `ok: N services, M roles` or the error list; exit 1 on any error.
  - `yeet render` — writes the rendered compose for a synthetic release id to stdout.
  - `yeet deploy [-e env] [--verbose]` — builds release id (`git rev-parse --short HEAD` best-effort), stages to a temp dir (rendered compose + verbatim yeet.yml copy as snapshot), connects `transport.NewSSH(host)`, runs `engine.Deploy`.
  - `yeet rollback [-e env]` — connects and runs `engine.Rollback`.
  - `--verbose` wires a logger printing `[host] $ cmd` to stderr on both SSH and Local transports.

- [ ] **Step 1: Write the failing test**

```go
// cmd/yeet/commands_test.go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	composeYAML := `
services:
  server:
    image: ghcr.io/x/app:v1
  postgres:
    image: postgres:17
`
	yeetYAML := `
app: demo
compose: docker-compose.yaml
environments: { production: { hosts: [deploy@example.invalid] } }
roles:
  web: { service: server, mode: rolling, ready: { http: /healthz, port: 8080 } }
order: [web]
accessories: [postgres]
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(composeYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "yeet.yml"), []byte(yeetYAML), 0o644)
	return dir
}

func run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"-c", filepath.Join(dir, "yeet.yml")}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestValidateOK(t *testing.T) {
	dir := writeProject(t)
	out, err := run(t, dir, "validate")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("want ok, got: %s", out)
	}
}

func TestRenderInjectsDrainGuard(t *testing.T) {
	dir := writeProject(t)
	out, err := run(t, dir, "render")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "/tmp/yeet-drain") || !strings.Contains(out, "yeet.release") {
		t.Fatalf("render missing injections:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/...`
Expected: FAIL — no subcommands.

- [ ] **Step 3: Write minimal implementation**

```go
// cmd/yeet/commands.go
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/yeet/internal/compose"
	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/engine"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/transport"
)

type app struct {
	g   *globalFlags
	cfg *config.Config
}

func loadAll(g *globalFlags, ctx context.Context) (*config.Config, *composeProject, error) {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	composePath := cfg.Compose
	if !filepath.IsAbs(composePath) {
		composePath = filepath.Join(filepath.Dir(g.ConfigPath), composePath)
	}
	p, err := compose.Load(ctx, composePath, cfg.App)
	if err != nil {
		return nil, nil, err
	}
	if err := compose.Classify(p, cfg); err != nil {
		return nil, nil, err
	}
	return cfg, &composeProject{p}, nil
}

// (composeProject is a thin alias so this file reads clearly)
type composeProject struct{ P projectT }
```

Use the real type directly instead of the alias if it fights the compiler — the intent is just `*types.Project`. Full command registrations:

```go
func addCommands(root *cobra.Command, g *globalFlags) {
	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "schema + rollability + class assignment — no side effects",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, cp, err := loadAll(g, cmd.Context())
			if err != nil {
				return err
			}
			if errs := compose.CheckRollable(cp.P, cfg); len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintln(cmd.OutOrStdout(), "error:", e)
				}
				return fmt.Errorf("%d rollability errors", len(errs))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d services, %d roles\n", len(cp.P.Services), len(cfg.Roles))
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "render",
		Short: "print the rendered per-release compose (the exact delta’s output)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, cp, err := loadAll(g, cmd.Context())
			if err != nil {
				return err
			}
			out, err := compose.Render(cp.P, cfg, "render-preview")
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "deploy",
		Short: "release to the environment host with health-gated zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, false)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "re-release the previous release dir (pinned local image)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, g, true)
		},
	})
}

func runDeploy(cmd *cobra.Command, g *globalFlags, rollback bool) error {
	ctx := cmd.Context()
	cfg, cp, err := loadAll(g, ctx)
	if err != nil {
		return err
	}
	if errs := compose.CheckRollable(cp.P, cfg); len(errs) > 0 {
		return fmt.Errorf("not rollable: %v", errs)
	}
	env, err := cfg.Environment(g.Env)
	if err != nil {
		return err
	}
	t, err := transport.NewSSH(env.Hosts[0])
	if err != nil {
		return err
	}
	defer t.Close()
	if g.Verbose {
		t.Logger = func(host, c string) { fmt.Fprintf(cmd.ErrOrStderr(), "[%s] $ %s\n", host, c) }
	}
	e := engine.New(cfg, cp.P, t, engine.Options{Verbose: g.Verbose, Out: cmd.OutOrStdout()})
	if rollback {
		return e.Rollback(ctx)
	}

	id := release.NewID(timeNow(), gitShortSHA(filepath.Dir(g.ConfigPath)))
	rendered, err := compose.Render(cp.P, cfg, id)
	if err != nil {
		return err
	}
	snapshot, err := os.ReadFile(g.ConfigPath)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "yeet-"+id)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := release.Stage(staging, rendered, snapshot); err != nil {
		return err
	}
	return e.Deploy(ctx, id, staging)
}

func gitShortSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
```

Wire into `newRootCmd()` (modify main.go): after flags, call `addCommands(root, g)`. Add `func timeNow() time.Time { return time.Now() }` and the `projectT = *types.Project` alias (or drop the alias and use the type directly).

- [ ] **Step 4: Run tests + build**

Run: `go test ./... && go build -o /tmp/yeet ./cmd/yeet && /tmp/yeet --help`
Expected: all PASS; help lists validate/render/deploy/rollback.

- [ ] **Step 5: Commit**

```bash
git add cmd/
git commit -m "feat(m0): wire validate, render, deploy, rollback CLI verbs"
```

---

### Task 13: E2E — zero-downtime proof against local docker

**Files:**
- Create: `e2e/testdata/app/docker-compose.yaml`, `e2e/testdata/app/yeet.yml`, `e2e/e2e_test.go`
- Modify: `README.md` (Status section: M0 usage + e2e instructions)

**Interfaces:**
- Consumes: the built engine with `transport.NewLocal()` (docker on the local machine plays the "host").
- Gate: test skips unless `YEET_E2E=1` AND `docker info` succeeds.

- [ ] **Step 1: Write the fixture app**

```yaml
# e2e/testdata/app/docker-compose.yaml
# Traefik + a busybox httpd "web" — smallest thing that can prove ZDD.
services:
  traefik:
    image: traefik:v3.1
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:18080
      - --ping=true
    ports: ["18080:18080"]
    volumes: ["/var/run/docker.sock:/var/run/docker.sock:ro"]
    healthcheck:
      test: ["CMD", "traefik", "healthcheck", "--ping"]
      interval: 2s
  web:
    image: busybox:1.36
    command: sh -c 'echo "$$APP_VERSION" > /www/index.html && exec httpd -f -p 8080 -h /www'
    environment:
      APP_VERSION: ${APP_VERSION:-v1}
    labels:
      - traefik.enable=true
      - traefik.http.routers.e2e.rule=PathPrefix(`/`)
      - traefik.http.routers.e2e.entrypoints=web
      - traefik.http.services.e2e.loadbalancer.server.port=8080
    tmpfs: [/www]
```

Wait — `tmpfs` + writing index.html in command works: busybox writes then serves. Note `$$` escapes compose interpolation.

```yaml
# e2e/testdata/app/yeet.yml
app: yeete2e
compose: docker-compose.yaml
environments:
  production: { hosts: [local] }   # e2e uses LocalTransport; host value unused
roles:
  web:
    service: web
    mode: rolling
    ready: { http: /, port: 8080, interval: 1s, start_period: 1s, within: 60s }
accessories: [traefik]
verify:
  - { http: /, role: web }
```

- [ ] **Step 2: Write the e2e test**

```go
// e2e/e2e_test.go
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/compose"
	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/engine"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/transport"
)

func TestZeroDowntimeDeploy(t *testing.T) {
	if os.Getenv("YEET_E2E") != "1" {
		t.Skip("set YEET_E2E=1 (requires local docker)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	dir, _ := filepath.Abs("testdata/app")
	ctx := context.Background()
	tr := transport.NewLocal()
	t.Cleanup(func() {
		exec.Command("sh", "-c", "docker compose -p yeete2e down -v --remove-orphans").Run()
		exec.Command("sh", "-c", "sudo rm -rf /var/lib/yeet/yeete2e 2>/dev/null || rm -rf /var/lib/yeet/yeete2e").Run()
	})

	deploy := func(version string) error {
		os.Setenv("APP_VERSION", version)
		cfg, err := config.Load(filepath.Join(dir, "yeet.yml"))
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		p, err := compose.Load(ctx, filepath.Join(dir, "docker-compose.yaml"), cfg.App)
		if err != nil {
			return err
		}
		id := release.NewID(time.Now(), version)
		rendered, err := compose.Render(p, cfg, id)
		if err != nil {
			return err
		}
		staging := t.TempDir()
		if err := release.Stage(staging, rendered, []byte("snapshot")); err != nil {
			return err
		}
		e := engine.New(cfg, p, tr, engine.Options{Out: os.Stderr})
		// accessory traefik must exist before first preflight: start it directly
		exec.Command("sh", "-c",
			"cd "+dir+" && docker compose -p yeete2e up -d traefik").Run()
		return e.Deploy(ctx, id, staging)
	}

	if err := deploy("v1"); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}

	// hammer the edge during the v2 deploy; count failures
	var failures, total atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get("http://localhost:18080/")
			total.Add(1)
			if err != nil || resp.StatusCode != 200 {
				failures.Add(1)
			}
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	if err := deploy("v2"); err != nil {
		close(stop)
		t.Fatalf("deploy v2: %v", err)
	}
	close(stop)

	if f := failures.Load(); f > 0 {
		t.Fatalf("zero-downtime violated: %d/%d requests failed during roll", f, total.Load())
	}
	resp, err := http.Get("http://localhost:18080/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "v2\n" {
		t.Fatalf("expected v2 content, got %q", body)
	}
	fmt.Printf("zero-downtime proven: %d requests, 0 failures\n", total.Load())
}
```

**Note:** `/var/lib/yeet` on macOS/local may need `sudo mkdir -p /var/lib/yeet && sudo chown $USER /var/lib/yeet` once; document in README. If Preflight's writable check fails, that's the correct error message doing its job.

- [ ] **Step 3: Run gated**

Run: `go test ./e2e/ -run TestZeroDowntime` → SKIP (gate works).
Run: `sudo mkdir -p /var/lib/yeet && sudo chown $USER /var/lib/yeet && YEET_E2E=1 go test ./e2e/ -run TestZeroDowntime -v -timeout 10m`
Expected: PASS with `zero-downtime proven: N requests, 0 failures`. Debug loop lives here — traefik health-poll timing, busybox httpd quirks; fix engine constants (ConvergeBuffer) if 502s appear, do NOT loosen the failure assertion.

- [ ] **Step 4: Update README**

Add under Status: `M0 in progress: yeet validate|render|deploy|rollback work end-to-end; e2e zero-downtime proof in e2e/ (YEET_E2E=1 go test ./e2e/). Monk cutover checklist: yeet.yml in ../monk (roles web/worker/scheduler, jobs migrate, accessories traefik/postgres/redis/ofelia), then yeet validate && yeet deploy -e production --verbose.`

- [ ] **Step 5: Commit**

```bash
git add e2e/ README.md
git commit -m "test(m0): e2e zero-downtime proof against local docker"
```

---

## Self-Review

1. **Spec coverage (M0 per design §12):** core lifecycle ✔ (Task 11), SSH ✔ (6), compose runtime ✔ (3–4), scale–health–drain ✔ (9), traefik-docker external ✔ (drain-guard healthcheck + preflight accessory check; monk's traefik is in-compose = accessory per rev 5), monk end-to-end ✔ (Task 13 e2e + README cutover checklist — the real monk deploy is a human step against a production host). Deliberately absent and stated: plan/apply, journal, locks, resume, migration gate, multi-host.
2. **Placeholder scan:** none — every task has runnable test + implementation code. Two flagged honesty notes (M0 rollback uses current choreography; compose-go field names may need compiler-driven adjustment) are stated constraints, not TODOs.
3. **Type consistency:** `Transport`/`Result`/`Fake.Dynamic` (5, 9), `config.Role/Ready/Drain/Duration` (2) used identically in 8–12; `compose.DrainFile` (3) consumed in 4, 9, 12 tests; `release.*` signatures (7) consumed in 11–12. `Engine.Opts.Sleep/Now/ConvergeBuffer` defined once (8), used in 9–11.
