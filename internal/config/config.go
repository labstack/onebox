// Package config models the stable onebox.run/v1 authoring contract and its
// normalized runtime view.
package config

import (
	"bytes"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/labstack/onebox/internal/target"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// Duration is a yaml-parseable time.Duration ("30s", "2m").
type Duration time.Duration

const APIVersion = "onebox.run/v1"

var lifecycleHookNames = [...]string{"bootstrap", "pre_release", "post_release", "post_deploy"}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil && strings.HasSuffix(s, "d") {
		days, dayErr := strconv.ParseUint(strings.TrimSuffix(s, "d"), 10, 64)
		maxDays := uint64(time.Duration(1<<63-1) / (24 * time.Hour))
		if dayErr == nil && days <= maxDays {
			v, err = time.Duration(days)*24*time.Hour, nil
		}
	}
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML emits the "30s" string form so a resolved config round-trips
// (the release snapshot is the resolved config, replayed by rollback).
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

type Config struct {
	APIVersion     string                 `yaml:"api_version"`
	App            string                 `yaml:"app,omitempty"`
	Compose        string                 `yaml:"compose,omitempty"`
	Environments   map[string]Environment `yaml:"environments"`
	Components     map[string]Component   `yaml:"components"`
	Deployment     Deployment             `yaml:"deployment,omitempty"`
	Runtime        Runtime                `yaml:"runtime,omitempty"`
	LifecycleHooks map[string]Hook        `yaml:"hooks,omitempty"`
	Verify         []VerifyCheck          `yaml:"verification,omitempty"`
	Proxy          Proxy                  `yaml:"proxy,omitempty"`
	Notify         *Notify                `yaml:"notifications,omitempty"`
	Registry       *Registry              `yaml:"registry,omitempty"`
	Secrets        *Secrets               `yaml:"secrets,omitempty"`
	Observability  Observability          `yaml:"observability,omitempty"`

	// Normalized runtime fields. The engine consumes these; they are derived
	// from Components/Deployment/Runtime and never accepted in v1 authoring.
	Roles       map[string]Role  `yaml:"-"`
	Order       []string         `yaml:"-"`
	Accessories []string         `yaml:"-"`
	Jobs        []string         `yaml:"-"`
	EnvFiles    []string         `yaml:"-"`
	Preflight   []PreflightCheck `yaml:"-"`
	Retain      int              `yaml:"-"`
	Migrations  string           `yaml:"-"`
	Hooks       map[string]Hook  `yaml:"-"`
	authoring   bool             `yaml:"-"`
}

type Environment struct {
	Target string            `yaml:"target,omitempty"`
	Policy EnvironmentPolicy `yaml:"policy,omitempty"`
	Hosts  []string          `yaml:"-"`
}

type EnvironmentPolicy struct {
	RequireApproval             *bool    `yaml:"require_approval,omitempty"`
	AllowAgentProposals         *bool    `yaml:"allow_agent_proposals,omitempty"`
	MinimumOneboxVersion        string   `yaml:"minimum_onebox_version,omitempty"`
	MinimumPlanSchema           string   `yaml:"minimum_plan_schema,omitempty"`
	RequireMigrationBackup      *bool    `yaml:"require_migration_backup,omitempty"`
	MigrationBackupMaxAge       Duration `yaml:"migration_backup_max_age,omitempty"`
	RequireMigrationRestoreTest *bool    `yaml:"require_migration_restore_test,omitempty"`
	MigrationBackupKeyMaterial  []string `yaml:"migration_backup_key_material,omitempty"`
}

func (p EnvironmentPolicy) ApprovalRequired() bool {
	return p.RequireApproval == nil || *p.RequireApproval
}

func (p EnvironmentPolicy) AgentProposalsAllowed() bool {
	return p.AllowAgentProposals == nil || *p.AllowAgentProposals
}

func (p EnvironmentPolicy) MigrationBackupRequired() bool {
	return p.RequireMigrationBackup != nil && *p.RequireMigrationBackup
}

func (p EnvironmentPolicy) MigrationRestoreTestRequired() bool {
	return p.RequireMigrationRestoreTest == nil || *p.RequireMigrationRestoreTest
}

type Component struct {
	Type        string               `yaml:"type"`
	Service     string               `yaml:"service,omitempty"`
	Deployment  *ComponentDeployment `yaml:"deployment,omitempty"`
	Readiness   *Ready               `yaml:"readiness,omitempty"`
	Drain       *Drain               `yaml:"drain,omitempty"`
	Command     *Hook                `yaml:"command,omitempty"`
	DataEffect  string               `yaml:"data_effect,omitempty"`
	Persistence *Persistence         `yaml:"persistence,omitempty"`
	Protection  *Protection          `yaml:"protection,omitempty"`
}

type ComponentDeployment struct {
	Strategy string `yaml:"strategy,omitempty"`
	Replicas int    `yaml:"replicas,omitempty"`
}

type Deployment struct {
	Order           []string `yaml:"order,omitempty"`
	RetainReleases  int      `yaml:"retain_releases,omitempty"`
	MigrationPolicy string   `yaml:"migration_policy,omitempty"`
}

type Runtime struct {
	// EnvFiles feed Compose interpolation and ship as runtime env files.
	EnvFiles  []string         `yaml:"env_files,omitempty"`
	Preflight []PreflightCheck `yaml:"preflight,omitempty"`
}

type Persistence struct {
	Mode    string   `yaml:"mode"`
	Volumes []string `yaml:"volumes,omitempty"`
}

type Protection struct {
	Backup       *BackupPolicy       `yaml:"backup,omitempty"`
	RestoreDrill *RestoreDrillPolicy `yaml:"restore_drill,omitempty"`
}

type BackupPolicy struct {
	Schedule      Schedule `yaml:"schedule"`
	RetentionDays int      `yaml:"retention_days"`
}

type RestoreDrillPolicy struct {
	Schedule Schedule `yaml:"schedule"`
}

// Schedule is a five-field cron expression evaluated in an explicit IANA
// timezone. Requiring both values now prevents future control planes from
// silently changing when a protection action runs.
type Schedule struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

type Observability struct {
	Logs    *LogPolicy    `yaml:"logs,omitempty"`
	Metrics *MetricPolicy `yaml:"metrics,omitempty"`
	Alerts  *AlertPolicy  `yaml:"alerts,omitempty"`
}

type LogPolicy struct {
	Enabled       bool `yaml:"enabled"`
	RetentionDays int  `yaml:"retention_days,omitempty"`
}

type MetricPolicy struct {
	Enabled bool `yaml:"enabled"`
}

type AlertPolicy struct {
	UnhealthyAfter Duration `yaml:"unhealthy_after"`
}

func (c *Config) Normalize() error {
	c.Roles = map[string]Role{}
	c.Accessories = nil
	c.Jobs = nil
	c.Order = append([]string(nil), c.Deployment.Order...)
	c.EnvFiles = append([]string(nil), c.Runtime.EnvFiles...)
	c.Preflight = append([]PreflightCheck(nil), c.Runtime.Preflight...)
	c.Retain = c.Deployment.RetainReleases
	if c.Retain <= 0 {
		c.Retain = 5
		c.Deployment.RetainReleases = c.Retain
	}
	if c.Deployment.MigrationPolicy == "" {
		c.Deployment.MigrationPolicy = "manual"
	}
	c.Migrations = ""
	if c.Deployment.MigrationPolicy == "expand-only" {
		c.Migrations = "expand-only"
	}
	c.Hooks = make(map[string]Hook, len(c.LifecycleHooks))
	for name, hook := range c.LifecycleHooks {
		c.Hooks[name] = hook
	}

	environmentNames := make([]string, 0, len(c.Environments))
	for name := range c.Environments {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		environment := c.Environments[name]
		environment.Hosts = nil
		if environment.Target != "" {
			environment.Hosts = []string{environment.Target}
		}
		c.Environments[name] = environment
	}

	names := make([]string, 0, len(c.Components))
	for name := range c.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		component := c.Components[name]
		service := component.Service
		if service == "" {
			service = name
			component.Service = service
			c.Components[name] = component
		}
		switch component.Type {
		case "application", "worker":
			deployment := component.Deployment
			if deployment == nil {
				deployment = &ComponentDeployment{}
				component.Deployment = deployment
				c.Components[name] = component
			}
			c.Roles[name] = Role{
				Service: service, Mode: deployment.Strategy, Replicas: deployment.Replicas,
				Ready: component.Readiness, Drain: component.Drain,
			}
		case "job":
			if isLifecycleHook(service) {
				return fmt.Errorf("components.%s.service: %q is reserved for a lifecycle hook", name, service)
			}
			c.Jobs = append(c.Jobs, service)
			if component.Command != nil {
				if _, exists := c.Hooks[service]; exists {
					return fmt.Errorf("components.%s.command conflicts with hooks.%s", name, service)
				}
				c.Hooks[service] = *component.Command
			}
		case "postgres", "mysql", "redis", "service":
			c.Accessories = append(c.Accessories, service)
		default:
			return fmt.Errorf("components.%s.type: unsupported component type %q", name, component.Type)
		}
	}
	return nil
}

func (c *Config) syncAuthoringFromRuntime() {
	if c.APIVersion == "" {
		c.APIVersion = APIVersion
	}
	for name, environment := range c.Environments {
		if environment.Target == "" && len(environment.Hosts) == 1 {
			environment.Target = environment.Hosts[0]
			c.Environments[name] = environment
		}
	}
	for _, name := range lifecycleHookNames {
		if hook, ok := c.Hooks[name]; ok {
			if c.LifecycleHooks == nil {
				c.LifecycleHooks = map[string]Hook{}
			}
			c.LifecycleHooks[name] = hook
		}
	}
	if len(c.Components) == 0 {
		c.Components = map[string]Component{}
		roleNames := make([]string, 0, len(c.Roles))
		for name := range c.Roles {
			roleNames = append(roleNames, name)
		}
		sort.Strings(roleNames)
		for _, name := range roleNames {
			role := c.Roles[name]
			componentType := "application"
			if strings.Contains(strings.ToLower(name), "worker") {
				componentType = "worker"
			}
			c.Components[name] = Component{
				Type: componentType, Service: role.Service,
				Deployment: &ComponentDeployment{Strategy: role.Mode, Replicas: role.Replicas},
				Readiness:  role.Ready, Drain: role.Drain,
			}
		}
		for _, service := range c.Accessories {
			c.Components[service] = Component{Type: "service", Service: service}
		}
		for _, service := range c.Jobs {
			component := Component{Type: "job", Service: service, DataEffect: "unknown"}
			if hook, ok := c.Hooks[service]; ok {
				hookCopy := hook
				component.Command = &hookCopy
			}
			c.Components[service] = component
		}
	}
	for name, component := range c.Components {
		if component.Type != "job" || component.Command != nil {
			continue
		}
		service := component.Service
		if service == "" {
			service = name
		}
		if hook, ok := c.Hooks[service]; ok {
			hookCopy := hook
			component.Command = &hookCopy
			c.Components[name] = component
		}
	}
	c.Deployment.Order = append([]string(nil), c.Order...)
	if c.Retain > 0 {
		c.Deployment.RetainReleases = c.Retain
	}
	if c.Migrations == "expand-only" {
		c.Deployment.MigrationPolicy = "expand-only"
	} else if c.Deployment.MigrationPolicy == "" {
		c.Deployment.MigrationPolicy = "manual"
	}
	c.Runtime.EnvFiles = append([]string(nil), c.EnvFiles...)
	c.Runtime.Preflight = append([]PreflightCheck(nil), c.Preflight...)
}

type Role struct {
	Service string `yaml:"service"`
	Mode    string `yaml:"mode"` // rolling | recreate
	// Replicas is the steady-state instance count (default 1). >1 runs a
	// load-balanced fleet named <service>-1..<service>-N; a rolling deploy
	// surges one new replica at a time. 0/absent means 1.
	Replicas int    `yaml:"replicas,omitempty"`
	Ready    *Ready `yaml:"ready,omitempty"`
	Drain    *Drain `yaml:"drain,omitempty"`
}

// Count is the resolved replica count (default 1).
func (r Role) Count() int {
	if r.Replicas < 1 {
		return 1
	}
	return r.Replicas
}

// StopGraceSeconds is the integer timeout used by `docker stop -t` and Compose
// recreate: drain.grace if set, else 30s (design §03's conservative default).
// A positive sub-second grace rounds UP to 1s — `docker stop -t` is integer
// seconds, and truncating (e.g. 500ms → 0) would mean an immediate SIGKILL, the
// opposite of a graceful stop.
func (r Role) StopGraceSeconds() int {
	if r.Drain != nil && r.Drain.Grace > 0 {
		return int(math.Ceil(time.Duration(r.Drain.Grace).Seconds()))
	}
	return 30
}

// HealthRetries is the consecutive-failure count Docker uses on the generated
// healthcheck before it flips health: ready.retries if set, else Docker's own
// default of 3. The drain step derives its wait budget from this, so the two
// can't drift (a high retries needs a proportionally longer drain budget).
func (r Role) HealthRetries() int {
	if r.Ready != nil && r.Ready.Retries > 0 {
		return r.Ready.Retries
	}
	return 3
}

type Ready struct {
	HTTP        string   `yaml:"http,omitempty"` // path, e.g. /healthz
	Exec        string   `yaml:"exec,omitempty"` // command run inside the container
	Port        int      `yaml:"port,omitempty"`
	Interval    Duration `yaml:"interval,omitempty"`     // generated-healthcheck cadence, default 2s
	StartPeriod Duration `yaml:"start_period,omitempty"` // default 5s
	Within      Duration `yaml:"within,omitempty"`       // overall gate timeout, default 120s
	// Retries is the generated healthcheck's consecutive-failure count before
	// Docker flips health (default: Docker's own default of 3). It governs how
	// fast the drain guard flips a container to `unhealthy` so the proxy drops
	// it: the flip takes Retries × Interval. Set retries: 1 for a fast drain.
	// Adopted (author-authored) healthchecks keep their own retries unless
	// ready.retries is set (it overrides the adopted count too).
	Retries int `yaml:"retries,omitempty"`
}

type Drain struct {
	Signal string   `yaml:"signal,omitempty"`
	Wait   Duration `yaml:"wait,omitempty"`
	// Grace is the SIGTERM→SIGKILL timeout: `docker stop -t` for rolling
	// retirement and `docker compose up --timeout` for recreate replacement
	// (default 30s). A prompt-exiting process never pays the full timeout.
	// Sub-second values round up to 1s.
	Grace Duration `yaml:"grace,omitempty"`
}

type VerifyCheck struct {
	HTTP               string                      `yaml:"http,omitempty"` // path, host-side against the container IP
	Exec               string                      `yaml:"exec,omitempty"`
	URL                string                      `yaml:"url,omitempty"` // runner-side edge check — advisory territory
	Role               string                      `yaml:"component,omitempty"`
	Port               int                         `yaml:"port,omitempty"`             // defaults to the role's ready.port
	Contains           string                      `yaml:"contains,omitempty"`         // for url checks: substring the body must contain
	Advisory           bool                        `yaml:"advisory,omitempty"`         // warn-only, never fails the deploy
	StatusCodes        []int                       `yaml:"status_codes,omitempty"`     // defaults to any 2xx response
	RequiredHeaders    map[string]string           `yaml:"required_headers,omitempty"` // exact response-header values
	JSONAssertions     []JSONAssertion             `yaml:"json_assertions,omitempty"`  // scalar equality at dotted paths
	MigrationRevisions *MigrationRevisionAssertion `yaml:"migration_revisions,omitempty"`
}

// MigrationRevisionAssertion compares provider-aware job evidence captured at
// the pre-release gate. It contains revision identifiers only, never database
// credentials or command output.
type MigrationRevisionAssertion struct {
	Job              string   `yaml:"job"`
	Provider         string   `yaml:"provider"`
	AppliedRevisions []string `yaml:"applied_revisions"`
}

type JSONAssertion struct {
	// Path is a dot-separated sequence of object keys and zero-based array
	// indexes, for example "service.ready" or "items.0.id".
	Path string `yaml:"path"`
	// Equals is restricted by the schema and Validate to a JSON scalar.
	Equals any `yaml:"equals"`
}

// PreflightCheck asserts that File exists and contains each key. Require keys
// must be present with a non-empty value; Present keys need only be declared
// (value may be empty — e.g. an optional feature toggle).
type PreflightCheck struct {
	File    string   `yaml:"file"`
	Require []string `yaml:"require,omitempty"`
	Present []string `yaml:"present,omitempty"`
}

// RunPreflight verifies every declared check against files under dir (paths in
// the config are relative to ob.yml). It fails on the first missing file or
// key so the operator learns exactly what's unset before anything ships.
func (c *Config) RunPreflight(dir string) error {
	for _, pc := range c.Preflight {
		path := pc.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("preflight: %s: %w", pc.File, err)
		}
		present, nonEmpty := envKeys(data)
		for _, k := range pc.Require {
			if !present[k] {
				return fmt.Errorf("preflight: %s is missing %s", pc.File, k)
			}
			if !nonEmpty[k] {
				return fmt.Errorf("preflight: %s has empty %s", pc.File, k)
			}
		}
		for _, k := range pc.Present {
			if !present[k] {
				return fmt.Errorf("preflight: %s is missing %s", pc.File, k)
			}
		}
	}
	return nil
}

// envKeys scans dotenv-style bytes into the set of declared keys and the subset
// with a non-empty value. A line is `KEY=value` (leading whitespace and an
// optional `export ` prefix tolerated); anything else is ignored.
func envKeys(data []byte) (present, nonEmpty map[string]bool) {
	present, nonEmpty = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		present[key] = true
		if strings.TrimSpace(line[eq+1:]) != "" {
			nonEmpty[key] = true
		}
	}
	return present, nonEmpty
}

// Hook is a user command run at a lifecycle seam — verbatim by design
// (§01: hooks are unplannable). String form runs on the host; the map form
// {run, local} can run on the runner (rsync-style publish steps).
type Hook struct {
	Run   string
	Local bool
}

func (h *Hook) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err == nil {
		h.Run = s
		return nil
	}
	var m struct {
		Run   string `yaml:"run"`
		Local bool   `yaml:"local"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	h.Run, h.Local = m.Run, m.Local
	return nil
}

// MarshalYAML emits the string form for host hooks and the {run, local} map for
// local ones, so a resolved config round-trips through the release snapshot.
func (h Hook) MarshalYAML() (any, error) {
	if !h.Local {
		return h.Run, nil
	}
	return map[string]any{"run": h.Run, "local": true}, nil
}

// Registry enables bootstrap's `docker login`; the password comes from the
// named env var and travels via stdin, never inside a command string.
type Registry struct {
	Server      string `yaml:"server"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

// Notify: outcome webhooks — a generic JSON POST fired when a mutating verb
// finishes. `on` picks which outcomes ("failure" is the default and the
// point: the journals are write-only; this is the push). Fail-open by
// design: a dead webhook warns, never blocks or fails the operation.
type Notify struct {
	Webhook string   `yaml:"webhook"`
	On      []string `yaml:"on,omitempty"`     // failure | success; default [failure]
	Format  string   `yaml:"format,omitempty"` // json (default; Slack-compatible) | text (plain line — ntfy-style topic endpoints)
}

// Secrets: a SOPS-encrypted flat YAML map, decrypted runner-side and shipped
// as a mode-600 env file inside each release dir (design §07).
type Secrets struct {
	Sops string `yaml:"sops"`
}

type Proxy struct {
	Kind    string `yaml:"kind,omitempty"`    // traefik-docker | none
	Managed bool   `yaml:"managed,omitempty"` // true: ob owns a HOST-scoped proxy (design: shared by every ob app on the box)
	Image   string `yaml:"image,omitempty"`   // default lives at the point of use (internal/proxy)
	Config  string `yaml:"config,omitempty"`  // dir with traefik.yml (+ dynamic.yml, .env); required when managed
	Network string `yaml:"network,omitempty"` // shared ingress network name; default at point of use
}

var (
	appName              = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	ident                = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	signalRe             = regexp.MustCompile(`^[A-Z0-9]+$`)
	registryServer       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.:-]*$`)
	registryUser         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	envVar               = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	executablePlanSchema = regexp.MustCompile(`^onebox\.run/executable-deploy-plan/v[0-9]+((alpha|beta)[0-9]+)?$`)
	headerName           = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	jsonPath             = regexp.MustCompile(`^[a-zA-Z0-9_-]+([.][a-zA-Z0-9_-]+)*$`)
	migrationRevisionID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)
)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".cue") {
		return LoadCUEBytes(b, path)
	}
	return LoadBytes(b, path)
}

// LoadBytes parses and CUE-validates config bytes (used by Load and by
// rollback's snapshot replay).
func LoadBytes(b []byte, filename string) (*Config, error) {
	if err := ValidateCUE(b, filename); err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	cfg.authoring = true
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	if cfg.Proxy.Managed && cfg.Proxy.Kind == "" {
		cfg.Proxy.Kind = "traefik-docker" // the one managed provider
	}
	if cfg.Notify != nil && len(cfg.Notify.On) == 0 {
		cfg.Notify.On = []string{"failure"} // pushes exist for the bad news
	}
	return cfg, nil
}

// DefaultApp derives an app name from the project directory when ob.yml omits
// `app`. Non-conforming characters are folded so the result usually matches the
// app-name rule; if it can't, Validate surfaces a clear error and the operator
// sets `app` explicitly.
func DefaultApp(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	base := strings.ToLower(filepath.Base(filepath.Dir(abs)))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// composeNames is compose's own precedence order for the default project file.
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// FindCompose returns the conventional compose file in dir when ob.yml omits
// `compose`. It falls back to the canonical name so the resulting error names a
// real path if nothing exists.
func FindCompose(dir string) string {
	for _, n := range composeNames {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	return "docker-compose.yaml"
}

// YAML marshals the resolved config — what the release snapshot stores so
// rollback replays the exact choreography (explicit roles, modes, order) that
// this deploy ran, not a re-inference against a possibly-changed compose file.
func (c *Config) YAML() ([]byte, error) {
	c.syncAuthoringFromRuntime()
	return yaml.Marshal(c)
}

func (c *Config) Validate() error {
	if c.authoring {
		if c.APIVersion != APIVersion {
			return fmt.Errorf("api_version: got %q, want %q", c.APIVersion, APIVersion)
		}
		if len(c.Components) == 0 {
			return fmt.Errorf("components: at least one required")
		}
	}
	if !appName.MatchString(c.App) {
		return fmt.Errorf("app: %q must match %s", c.App, appName)
	}
	if c.App == "ob-proxy" {
		// reserved: the managed proxy's compose project — an app with this
		// name would collide with the proxy's container queries
		return fmt.Errorf("app: %q is reserved for the managed proxy", c.App)
	}
	if c.Compose == "" {
		return fmt.Errorf("compose: required")
	}
	if len(c.Environments) == 0 {
		return fmt.Errorf("environments: at least one required")
	}
	for name, e := range c.Environments {
		if !ident.MatchString(name) {
			return fmt.Errorf("environments: name %q must match %s", name, ident)
		}
		if len(e.Hosts) != 1 {
			return fmt.Errorf("environments.%s: ob is single-host by design — exactly one host per environment, got %d", name, len(e.Hosts))
		}
		if c.authoring && e.Target == "" {
			return fmt.Errorf("environments.%s.target: required", name)
		}
		if minimum := strings.TrimSpace(e.Policy.MinimumOneboxVersion); minimum != "" {
			normalized := minimum
			if !strings.HasPrefix(normalized, "v") {
				normalized = "v" + normalized
			}
			if !semver.IsValid(normalized) {
				return fmt.Errorf("environments.%s.policy.minimum_onebox_version: %q is not a semantic version", name, minimum)
			}
		}
		if minimum := strings.TrimSpace(e.Policy.MinimumPlanSchema); minimum != "" && !executablePlanSchema.MatchString(minimum) {
			return fmt.Errorf("environments.%s.policy.minimum_plan_schema: %q is not an executable deploy plan schema", name, minimum)
		}
		backupRequired := e.Policy.MigrationBackupRequired()
		if backupRequired && e.Policy.MigrationBackupMaxAge <= 0 {
			return fmt.Errorf("environments.%s.policy.migration_backup_max_age: must be positive when require_migration_backup is true", name)
		}
		if backupRequired && !e.Policy.ApprovalRequired() {
			return fmt.Errorf("environments.%s.policy: require_migration_backup requires require_approval so overrides cannot bypass a strong approval ceremony", name)
		}
		if !backupRequired && (e.Policy.MigrationBackupMaxAge != 0 || e.Policy.RequireMigrationRestoreTest != nil || len(e.Policy.MigrationBackupKeyMaterial) > 0) {
			return fmt.Errorf("environments.%s.policy: migration backup settings require require_migration_backup: true", name)
		}
		seenKeyMaterial := make(map[string]bool, len(e.Policy.MigrationBackupKeyMaterial))
		for _, material := range e.Policy.MigrationBackupKeyMaterial {
			if !ident.MatchString(material) {
				return fmt.Errorf("environments.%s.policy.migration_backup_key_material: %q must match %s", name, material, ident)
			}
			if seenKeyMaterial[material] {
				return fmt.Errorf("environments.%s.policy.migration_backup_key_material: duplicate %q", name, material)
			}
			seenKeyMaterial[material] = true
		}
		if e.Target != "" {
			parsed, err := target.Parse(e.Target)
			if err != nil {
				return fmt.Errorf("environments.%s.target: %w", name, err)
			}
			if parsed.Host == "CHANGE-ME" {
				return fmt.Errorf("environments.%s.target: replace the scaffold CHANGE-ME value", name)
			}
		}
	}
	claimedServices := map[string]string{}
	for name, component := range c.Components {
		if !ident.MatchString(name) {
			return fmt.Errorf("components: name %q must match %s", name, ident)
		}
		if !ident.MatchString(component.Service) {
			return fmt.Errorf("components.%s.service: %q must match %s", name, component.Service, ident)
		}
		if previous, exists := claimedServices[component.Service]; exists {
			return fmt.Errorf("components.%s.service: %q is already claimed by component %q", name, component.Service, previous)
		}
		claimedServices[component.Service] = name
		switch component.Type {
		case "application", "worker":
			if component.Deployment == nil || (component.Deployment.Strategy != "rolling" && component.Deployment.Strategy != "recreate") {
				return fmt.Errorf("components.%s.deployment.strategy: must be rolling|recreate", name)
			}
			if component.Deployment.Replicas < 0 {
				return fmt.Errorf("components.%s.deployment.replicas: must be positive when set", name)
			}
		case "job":
			if component.DataEffect != "none" && component.DataEffect != "migration" && component.DataEffect != "unknown" {
				return fmt.Errorf("components.%s.data_effect: must be none|migration|unknown", name)
			}
			if isLifecycleHook(component.Service) {
				return fmt.Errorf("components.%s.service: %q is reserved for a lifecycle hook", name, component.Service)
			}
			if component.Command != nil && component.Command.Run == "" {
				return fmt.Errorf("components.%s.command.run: must not be empty", name)
			}
		case "postgres", "mysql", "redis":
			if component.Persistence == nil {
				return fmt.Errorf("components.%s.persistence: required for %s", name, component.Type)
			}
		case "service":
		default:
			return fmt.Errorf("components.%s.type: unsupported component type %q", name, component.Type)
		}
		if err := validatePersistence("components."+name+".persistence", component.Persistence); err != nil {
			return err
		}
		if err := validateProtection("components."+name+".protection", component.Protection); err != nil {
			return err
		}
	}
	if len(c.Roles) == 0 {
		return fmt.Errorf("components: at least one application or worker required")
	}
	inOrder := map[string]bool{}
	for _, r := range c.Order {
		if _, ok := c.Roles[r]; !ok {
			return fmt.Errorf("deployment.order: %q is not an application or worker component", r)
		}
		if inOrder[r] {
			return fmt.Errorf("deployment.order: duplicate component %q", r)
		}
		inOrder[r] = true
	}
	for name, r := range c.Roles {
		if !ident.MatchString(name) {
			return fmt.Errorf("roles: name %q must match %s", name, ident)
		}
		if !ident.MatchString(r.Service) {
			return fmt.Errorf("roles.%s: service %q must match %s", name, r.Service, ident)
		}
		if r.Mode != "rolling" && r.Mode != "recreate" {
			return fmt.Errorf("roles.%s: mode must be rolling|recreate, got %q", name, r.Mode)
		}
		// rolling needs a readiness contract, but it may be ADOPTED from the
		// compose file's own healthcheck (design §03) — that cross-file check
		// lives in compose.CheckRollable, which can see both files.
		if r.Ready != nil {
			hasHTTP, hasExec := r.Ready.HTTP != "", r.Ready.Exec != ""
			if hasHTTP == hasExec {
				return fmt.Errorf("components.%s.readiness: exactly one of http or exec is required", name)
			}
			if hasHTTP && r.Ready.Port == 0 {
				return fmt.Errorf("components.%s.readiness: http requires port", name)
			}
			if hasExec && r.Ready.Port != 0 {
				return fmt.Errorf("components.%s.readiness: port is only valid with http", name)
			}
			if r.Ready.Interval < 0 || r.Ready.StartPeriod < 0 || r.Ready.Within < 0 {
				return fmt.Errorf("components.%s.readiness: durations must not be negative", name)
			}
		}
		if r.Drain != nil && r.Drain.Signal != "" && !signalRe.MatchString(r.Drain.Signal) {
			return fmt.Errorf("components.%s.drain.signal: %q must match %s", name, r.Drain.Signal, signalRe)
		}
		if r.Ready != nil && r.Ready.Retries < 0 {
			return fmt.Errorf("components.%s.readiness.retries: must be >= 1 (0/absent = Docker default 3)", name)
		}
		if r.Drain != nil && (r.Drain.Wait < 0 || r.Drain.Grace < 0) {
			return fmt.Errorf("components.%s.drain: durations must not be negative", name)
		}
		if !inOrder[name] {
			return fmt.Errorf("deployment.order: must include every application and worker; missing %q", name)
		}
	}
	for _, a := range c.Accessories {
		if !ident.MatchString(a) {
			return fmt.Errorf("accessories: %q must match %s", a, ident)
		}
	}
	for _, j := range c.Jobs {
		if !ident.MatchString(j) {
			return fmt.Errorf("jobs: %q must match %s", j, ident)
		}
	}
	for name, hook := range c.LifecycleHooks {
		if !isLifecycleHook(name) {
			return fmt.Errorf("hooks.%s: unsupported lifecycle hook", name)
		}
		if hook.Run == "" {
			return fmt.Errorf("hooks.%s.run: must not be empty", name)
		}
	}
	if c.Deployment.MigrationPolicy != "" && c.Deployment.MigrationPolicy != "manual" && c.Deployment.MigrationPolicy != "expand-only" {
		return fmt.Errorf("deployment.migration_policy: must be manual|expand-only")
	}
	for i, check := range c.Verify {
		kinds := 0
		if check.HTTP != "" {
			kinds++
		}
		if check.Exec != "" {
			kinds++
		}
		if check.URL != "" {
			kinds++
		}
		if check.MigrationRevisions != nil {
			kinds++
		}
		path := fmt.Sprintf("verification.%d", i)
		if kinds != 1 {
			return fmt.Errorf("%s: exactly one of http, exec, url, or migration_revisions is required", path)
		}
		if assertion := check.MigrationRevisions; assertion != nil {
			if check.Role != "" || check.Port != 0 || check.Contains != "" || check.Advisory ||
				len(check.StatusCodes) > 0 || len(check.RequiredHeaders) > 0 || len(check.JSONAssertions) > 0 {
				return fmt.Errorf("%s: migration_revisions cannot be combined with component, port, or URL assertion fields", path)
			}
			if !ident.MatchString(assertion.Job) {
				return fmt.Errorf("%s.migration_revisions.job: must match %s", path, ident)
			}
			migrationJob := false
			for name, component := range c.Components {
				service := component.Service
				if service == "" {
					service = name
				}
				if component.Type == "job" && component.DataEffect == "migration" && service == assertion.Job {
					migrationJob = true
					break
				}
			}
			if !migrationJob {
				return fmt.Errorf("%s.migration_revisions.job: %q is not a configured migration job service", path, assertion.Job)
			}
			if !ident.MatchString(assertion.Provider) {
				return fmt.Errorf("%s.migration_revisions.provider: must match %s", path, ident)
			}
			if len(assertion.AppliedRevisions) == 0 {
				return fmt.Errorf("%s.migration_revisions.applied_revisions: at least one revision is required", path)
			}
			seen := make(map[string]bool, len(assertion.AppliedRevisions))
			for _, revision := range assertion.AppliedRevisions {
				if !migrationRevisionID.MatchString(revision) {
					return fmt.Errorf("%s.migration_revisions.applied_revisions: invalid revision identifier", path)
				}
				if seen[revision] {
					return fmt.Errorf("%s.migration_revisions.applied_revisions: duplicate revision %q", path, revision)
				}
				seen[revision] = true
			}
			continue
		}
		if check.URL != "" {
			u, err := url.ParseRequestURI(check.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("%s.url: must be an absolute HTTP(S) URL", path)
			}
			if check.Role != "" || check.Port != 0 {
				return fmt.Errorf("%s: component and port are not valid for a URL check", path)
			}
			seenStatus := make(map[int]bool, len(check.StatusCodes))
			for _, status := range check.StatusCodes {
				if status < 100 || status > 599 {
					return fmt.Errorf("%s.status_codes: status %d must be between 100 and 599", path, status)
				}
				if seenStatus[status] {
					return fmt.Errorf("%s.status_codes: duplicate status %d", path, status)
				}
				seenStatus[status] = true
			}
			headerNames := make([]string, 0, len(check.RequiredHeaders))
			for name := range check.RequiredHeaders {
				headerNames = append(headerNames, name)
			}
			sort.Strings(headerNames)
			seenHeaders := make(map[string]string, len(headerNames))
			for _, name := range headerNames {
				if !headerName.MatchString(name) {
					return fmt.Errorf("%s.required_headers: invalid HTTP header name %q", path, name)
				}
				folded := strings.ToLower(name)
				if previous, exists := seenHeaders[folded]; exists {
					return fmt.Errorf("%s.required_headers: header %q is configured more than once (also %q)", path, name, previous)
				}
				seenHeaders[folded] = name
				if strings.ContainsAny(check.RequiredHeaders[name], "\r\n") {
					return fmt.Errorf("%s.required_headers.%s: value must not contain a newline", path, name)
				}
			}
			for assertionIndex, assertion := range check.JSONAssertions {
				assertionPath := fmt.Sprintf("%s.json_assertions.%d", path, assertionIndex)
				if !jsonPath.MatchString(assertion.Path) {
					return fmt.Errorf("%s.path: must be a dot-separated JSON path", assertionPath)
				}
				if !isJSONScalar(assertion.Equals) {
					return fmt.Errorf("%s.equals: must be a string, number, boolean, or null", assertionPath)
				}
			}
			continue
		}
		role, ok := c.Roles[check.Role]
		if !ok {
			return fmt.Errorf("%s.component: unknown application or worker %q", path, check.Role)
		}
		if check.Contains != "" || check.Advisory || len(check.StatusCodes) > 0 || len(check.RequiredHeaders) > 0 || len(check.JSONAssertions) > 0 {
			return fmt.Errorf("%s: contains, advisory, status_codes, required_headers, and json_assertions are only valid for URL checks", path)
		}
		if check.Exec != "" && check.Port != 0 {
			return fmt.Errorf("%s.port: only valid for an HTTP check", path)
		}
		if check.HTTP != "" && check.Port == 0 && (role.Ready == nil || role.Ready.Port == 0) {
			return fmt.Errorf("%s.port: required when component readiness has no HTTP port", path)
		}
	}
	if c.Registry != nil {
		if !registryServer.MatchString(c.Registry.Server) {
			return fmt.Errorf("registry.server: %q is not option-safe", c.Registry.Server)
		}
		if !registryUser.MatchString(c.Registry.Username) {
			return fmt.Errorf("registry.username: %q is not option-safe", c.Registry.Username)
		}
		if !envVar.MatchString(c.Registry.PasswordEnv) {
			return fmt.Errorf("registry.password_env: %q is not an environment variable name", c.Registry.PasswordEnv)
		}
	}
	if c.Observability.Logs != nil && c.Observability.Logs.RetentionDays < 0 {
		return fmt.Errorf("observability.logs.retention_days: must be positive when set")
	}
	if c.Observability.Alerts != nil && c.Observability.Alerts.UnhealthyAfter <= 0 {
		return fmt.Errorf("observability.alerts.unhealthy_after: must be positive")
	}
	if c.Proxy.Managed {
		if c.Proxy.Kind == "none" {
			return fmt.Errorf("proxy: managed: true contradicts kind: none — a managed proxy is one ob runs")
		}
		if c.Proxy.Config == "" {
			return fmt.Errorf("proxy.config: required when managed — the dir holding traefik.yml (+ dynamic.yml, .env)")
		}
	}
	// ready timing defaults live at the point of use (engine.readyTiming,
	// compose.Render) — injecting them here would stomp timings ADOPTED from
	// the compose file's own healthcheck.
	return nil
}

func isJSONScalar(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Float32, reflect.Float64:
		value := v.Float()
		return !math.IsNaN(value) && !math.IsInf(value, 0)
	default:
		return false
	}
}

func isLifecycleHook(name string) bool {
	for _, allowed := range lifecycleHookNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func validatePersistence(path string, persistence *Persistence) error {
	if persistence == nil {
		return nil
	}
	if persistence.Mode != "durable" && persistence.Mode != "ephemeral" && persistence.Mode != "external" {
		return fmt.Errorf("%s.mode: must be durable|ephemeral|external", path)
	}
	seen := map[string]bool{}
	for _, volume := range persistence.Volumes {
		if !ident.MatchString(volume) {
			return fmt.Errorf("%s.volumes: %q must match %s", path, volume, ident)
		}
		if seen[volume] {
			return fmt.Errorf("%s.volumes: %q appears more than once", path, volume)
		}
		seen[volume] = true
	}
	return nil
}

func validateProtection(path string, protection *Protection) error {
	if protection == nil {
		return nil
	}
	if protection.Backup == nil && protection.RestoreDrill == nil {
		return fmt.Errorf("%s: backup or restore_drill is required", path)
	}
	if protection.Backup != nil {
		if protection.Backup.RetentionDays <= 0 {
			return fmt.Errorf("%s.backup.retention_days: must be positive", path)
		}
		if err := validateSchedule(path+".backup.schedule", protection.Backup.Schedule); err != nil {
			return err
		}
	}
	if protection.RestoreDrill != nil {
		if err := validateSchedule(path+".restore_drill.schedule", protection.RestoreDrill.Schedule); err != nil {
			return err
		}
	}
	return nil
}

func validateSchedule(path string, schedule Schedule) error {
	if err := validateCron(schedule.Cron); err != nil {
		return fmt.Errorf("%s.cron: %w", path, err)
	}
	if schedule.Timezone == "" {
		return fmt.Errorf("%s.timezone: required", path)
	}
	if _, err := time.LoadLocation(schedule.Timezone); err != nil {
		return fmt.Errorf("%s.timezone: unknown IANA timezone %q", path, schedule.Timezone)
	}
	return nil
}

func validateCron(expression string) error {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return fmt.Errorf("must be a five-field numeric cron expression")
	}
	limits := [...]struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day-of-month", 1, 31},
		{"month", 1, 12},
		{"day-of-week", 0, 7},
	}
	for i, field := range fields {
		if err := validateCronField(field, limits[i].min, limits[i].max); err != nil {
			return fmt.Errorf("invalid %s field %q: %w", limits[i].name, field, err)
		}
	}
	return nil
}

func validateCronField(field string, min, max int) error {
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return fmt.Errorf("empty list item")
		}
		parts := strings.Split(item, "/")
		if len(parts) > 2 || parts[0] == "" {
			return fmt.Errorf("expected value, range, wildcard, or optional step")
		}
		if len(parts) == 2 {
			step, err := strconv.Atoi(parts[1])
			if err != nil || step <= 0 {
				return fmt.Errorf("step must be a positive integer")
			}
		}
		base := parts[0]
		if base == "*" {
			continue
		}
		rangeParts := strings.Split(base, "-")
		if len(rangeParts) > 2 {
			return fmt.Errorf("range has too many bounds")
		}
		start, err := strconv.Atoi(rangeParts[0])
		if err != nil || start < min || start > max {
			return fmt.Errorf("value must be between %d and %d", min, max)
		}
		if len(rangeParts) == 2 {
			end, err := strconv.Atoi(rangeParts[1])
			if err != nil || end < min || end > max || end < start {
				return fmt.Errorf("range end must be between %d and %d and not precede its start", min, max)
			}
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
