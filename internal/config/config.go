// Package config models yeet.yml (M0 subset — plain YAML; CUE validation is M1).
package config

import (
	"bytes"
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
	Hooks        map[string]Hook        `yaml:"hooks"`
	Verify       []VerifyCheck          `yaml:"verify"`
	Proxy        Proxy                  `yaml:"proxy"`
	Registry     *Registry              `yaml:"registry"`
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
	HTTP     string `yaml:"http"` // path, host-side against the container IP
	Exec     string `yaml:"exec"`
	URL      string `yaml:"url"` // runner-side edge check — advisory territory
	Role     string `yaml:"role"`
	Port     int    `yaml:"port"`     // defaults to the role's ready.port
	Contains string `yaml:"contains"` // for url checks: substring the body must contain
	Advisory bool   `yaml:"advisory"` // warn-only, never fails the deploy
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

// Registry enables bootstrap's `docker login`; the password comes from the
// named env var and travels via stdin, never inside a command string.
type Registry struct {
	Server      string `yaml:"server"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
}

type Proxy struct {
	Kind    string `yaml:"kind"`    // traefik-docker | none (M0: informational)
	Managed bool   `yaml:"managed"` // M0: must be false
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
		if !ident.MatchString(name) {
			return fmt.Errorf("roles: name %q must match %s", name, ident)
		}
		if !ident.MatchString(r.Service) {
			return fmt.Errorf("roles.%s: service %q must match %s", name, r.Service, ident)
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
	for name, r := range c.Roles {
		if r.Ready != nil {
			rd := *r.Ready
			if rd.Interval == 0 {
				rd.Interval = Duration(5 * time.Second)
			}
			if rd.StartPeriod == 0 {
				rd.StartPeriod = Duration(5 * time.Second)
			}
			if rd.Within == 0 {
				rd.Within = Duration(120 * time.Second)
			}
			r.Ready = &rd
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
