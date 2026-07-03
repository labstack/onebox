// Package config models yeet.yml (M0 subset — plain YAML; CUE validation is M1).
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// MarshalYAML emits the "30s" string form so a resolved config round-trips
// (the release snapshot is the resolved config, replayed by rollback).
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

type Config struct {
	App          string                 `yaml:"app,omitempty"`
	Compose      string                 `yaml:"compose,omitempty"`
	Environments map[string]Environment `yaml:"environments"`
	Roles        map[string]Role        `yaml:"roles"`
	Order        []string               `yaml:"order,omitempty"`
	Accessories  []string               `yaml:"accessories,omitempty"`
	Jobs         []string               `yaml:"jobs,omitempty"`
	Hooks        map[string]Hook        `yaml:"hooks,omitempty"`
	Verify       []VerifyCheck          `yaml:"verify,omitempty"`
	Proxy        Proxy                  `yaml:"proxy,omitempty"`
	Registry     *Registry              `yaml:"registry,omitempty"`
	Secrets      *Secrets               `yaml:"secrets,omitempty"`
	Retain       int                    `yaml:"retain,omitempty"`
	// Migrations "expand-only" is the operator's informed promise that old
	// code tolerates the new schema — it permits auto-rollback past the
	// migration gate (design §06).
	Migrations string `yaml:"migrations,omitempty"`
}

type Environment struct {
	Hosts []string `yaml:"hosts"`
}

type Role struct {
	Service   string `yaml:"service"`
	Mode      string `yaml:"mode"` // rolling | recreate
	Singleton bool   `yaml:"singleton,omitempty"`
	Ready     *Ready `yaml:"ready,omitempty"`
	Drain     *Drain `yaml:"drain,omitempty"`
}

type Ready struct {
	HTTP        string   `yaml:"http,omitempty"` // path, e.g. /healthz
	Exec        string   `yaml:"exec,omitempty"` // command run inside the container
	Port        int      `yaml:"port,omitempty"`
	Interval    Duration `yaml:"interval,omitempty"`     // default 5s
	StartPeriod Duration `yaml:"start_period,omitempty"` // default 5s
	Within      Duration `yaml:"within,omitempty"`       // overall gate timeout, default 120s
}

type Drain struct {
	Signal string   `yaml:"signal,omitempty"`
	Wait   Duration `yaml:"wait,omitempty"`
}

type VerifyCheck struct {
	HTTP     string `yaml:"http,omitempty"` // path, host-side against the container IP
	Exec     string `yaml:"exec,omitempty"`
	URL      string `yaml:"url,omitempty"` // runner-side edge check — advisory territory
	Role     string `yaml:"role,omitempty"`
	Port     int    `yaml:"port,omitempty"`     // defaults to the role's ready.port
	Contains string `yaml:"contains,omitempty"` // for url checks: substring the body must contain
	Advisory bool   `yaml:"advisory,omitempty"` // warn-only, never fails the deploy
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

// Secrets: a SOPS-encrypted flat YAML map, decrypted runner-side and shipped
// as a mode-600 env file inside each release dir (design §07).
type Secrets struct {
	Sops string `yaml:"sops"`
}

type Proxy struct {
	Kind    string `yaml:"kind,omitempty"`    // traefik-docker | none (M0: informational)
	Managed bool   `yaml:"managed,omitempty"` // M0: must be false
}

var (
	appName  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	ident    = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	signalRe = regexp.MustCompile(`^[A-Z0-9]+$`)
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
	cfg := &Config{Retain: 5}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	if cfg.Retain <= 0 {
		cfg.Retain = 5
	}
	return cfg, nil
}

// DefaultApp derives an app name from the project directory when yeet.yml omits
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

// FindCompose returns the conventional compose file in dir when yeet.yml omits
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
func (c *Config) YAML() ([]byte, error) { return yaml.Marshal(c) }

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
			return fmt.Errorf("environments.%s: yeet is single-host by design — exactly one host per environment, got %d", name, len(e.Hosts))
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
		if r.Ready != nil && r.Ready.HTTP != "" && r.Ready.Port == 0 {
			return fmt.Errorf("roles.%s: ready.http requires port", name)
		}
		if r.Drain != nil && r.Drain.Signal != "" && !signalRe.MatchString(r.Drain.Signal) {
			return fmt.Errorf("roles.%s: drain.signal %q must match %s", name, r.Drain.Signal, signalRe)
		}
		if !inOrder[name] {
			return fmt.Errorf("order: must include every role; missing %q", name)
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
	if c.Proxy.Managed {
		return fmt.Errorf("proxy.managed: true is M1+ (bootstrap); M0 supports external proxies only")
	}
	// ready timing defaults live at the point of use (engine.readyTiming,
	// compose.Render) — injecting them here would stomp timings ADOPTED from
	// the compose file's own healthcheck.
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
