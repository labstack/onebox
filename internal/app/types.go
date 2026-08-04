// Package app loads the onebox.run/v1 declarative authoring contract: one
// application, its workloads, the services it needs, and how a release rolls
// out.
//
// It is not called `project` because Compose already owns that word. A reader
// of the execution path holds `Spec *app.Resolved` next to `Compose
// *ctypes.Project` and never has to ask which project is meant. In prose the
// file is still the project file — there is no second meaning for a user to
// trip over there.
//
// The pipeline is fixed and its order is load-bearing: parse, expand shorthand,
// validate against the embedded CUE schema, then apply cross-field rules the
// schema cannot express. Expansion runs before validation because the schema
// describes the normalised form — a discriminator left to a default keeps every
// branch of a CUE disjunction alive, so `role` must be present before the
// schema sees it.
package app

// Spec is the normalised form of a project file. Every field is concrete:
// defaults have been applied by the schema and shorthand has been expanded, so
// consumers never have to ask whether a value was stated or derived.
type Spec struct {
	// Dir is the directory holding the project file. Every repository path
	// resolves against it, so loading the same project from two working
	// directories yields the same runtime.
	Dir string `json:"-"`

	APIVersion string `json:"api_version"`
	// Name is the application's name. Spelled Name rather than App because
	// inside a package called app, `spec.App` is a stutter and every caller
	// then writes `.App.App`. The authored key is still `app:`.
	Name         string                 `json:"app"`
	BasePath     string                 `json:"base_path"`
	Environments map[string]Environment `json:"environments"`
	Workloads    map[string]Workload    `json:"workloads,omitempty"`
	Services     map[string]Service     `json:"services,omitempty"`
	Files        []string               `json:"files,omitempty"`

	Deployment    Deployment              `json:"deployment"`
	Runtime       *Runtime                `json:"runtime,omitempty"`
	Hooks         map[string]Command      `json:"hooks,omitempty"`
	Verification  []Verification          `json:"verification,omitempty"`
	Notifications map[string]Notification `json:"notifications,omitempty"`
	Registries    map[string]Registry     `json:"registries,omitempty"`
	Proxy         Proxy                   `json:"proxy"`
	Secrets       map[string]Secret       `json:"secrets,omitempty"`
	Observability *Observability          `json:"observability,omitempty"`

	// rawExpanded is the authored input after shorthand expansion, kept so a
	// value's origin can be reported without threading a marker through every
	// field of the model.
	rawExpanded map[string]any

	// derivedPaths marks canonical paths the author did not write where they
	// now appear: moved by shorthand expansion, or injected by normalisation.
	derivedPaths map[string]Origin
}

type Environment struct {
	Server    Server     `json:"server"`
	BasePath  string     `json:"base_path,omitempty"`
	Policy    Policy     `json:"policy"`
	Overrides *Overrides `json:"overrides,omitempty"`
}

// Server is a scalar `user@host` or an object. Both decode here.
type Server struct {
	Host string `json:"host"`
	User string `json:"user,omitempty"`
	Port int    `json:"port,omitempty"`
}

type Policy struct {
	RequireApproval             bool     `json:"require_approval"`
	AllowAgentProposals         bool     `json:"allow_agent_proposals"`
	MinimumOneboxVersion        string   `json:"minimum_onebox_version,omitempty"`
	MinimumPlanSchema           string   `json:"minimum_plan_schema,omitempty"`
	RequireMigrationBackup      bool     `json:"require_migration_backup"`
	MigrationBackupMaxAge       string   `json:"migration_backup_max_age,omitempty"`
	RequireMigrationRestoreTest bool     `json:"require_migration_restore_test"`
	MigrationBackupKeyMaterial  []string `json:"migration_backup_key_material,omitempty"`
}

type Overrides struct {
	Workloads map[string]map[string]any `json:"workloads,omitempty"`
	Services  map[string]map[string]any `json:"services,omitempty"`
}

type Workload struct {
	Role     string `json:"role"`
	Build    *Build `json:"build,omitempty"`
	Image    *Image `json:"image,omitempty"`
	Compose  string `json:"compose,omitempty"`
	Command  any    `json:"command,omitempty"`
	Replicas int    `json:"replicas"`
	Strategy string `json:"strategy,omitempty"`

	Domain string  `json:"domain,omitempty"`
	Port   int     `json:"port,omitempty"`
	Routes []Route `json:"routes,omitempty"`

	Health      *Health         `json:"health,omitempty"`
	Drain       *Drain          `json:"drain,omitempty"`
	Resources   *Resources      `json:"resources,omitempty"`
	Env         map[string]any  `json:"env,omitempty"`
	EnvFiles    []string        `json:"env_files,omitempty"`
	Volumes     []Volume        `json:"volumes,omitempty"`
	Ports       []PublishedPort `json:"ports,omitempty"`
	Persistence *Persistence    `json:"persistence,omitempty"`
	Needs       []Need          `json:"needs,omitempty"`

	Entrypoint any            `json:"entrypoint,omitempty"`
	User       string         `json:"user,omitempty"`
	Hostname   string         `json:"hostname,omitempty"`
	WorkingDir string         `json:"working_dir,omitempty"`
	Init       *bool          `json:"init,omitempty"`
	TTY        *bool          `json:"tty,omitempty"`
	StdinOpen  *bool          `json:"stdin_open,omitempty"`
	ExtraHosts []string       `json:"extra_hosts,omitempty"`
	Labels     map[string]any `json:"labels,omitempty"`
	Logging    *Logging       `json:"logging,omitempty"`

	// Job only.
	Run        string    `json:"run,omitempty"`
	DataEffect string    `json:"data_effect,omitempty"`
	Schedule   *Schedule `json:"schedule,omitempty"`
}

type Build struct {
	Context    string         `json:"context"`
	Dockerfile string         `json:"dockerfile,omitempty"`
	Target     string         `json:"target,omitempty"`
	Args       map[string]any `json:"args,omitempty"`
	Platform   string         `json:"platform,omitempty"`
}

type Image struct {
	Reference string `json:"reference"`
	Platform  string `json:"platform,omitempty"`
	Pull      string `json:"pull"`
	Registry  string `json:"registry,omitempty"`
}

type Route struct {
	Domain     string `json:"domain"`
	Path       string `json:"path"`
	Port       int    `json:"port"`
	Entrypoint string `json:"entrypoint"`
	Protocol   string `json:"protocol"`
	Scheme     string `json:"scheme"`
	TLS        string `json:"tls"`
}

type Health struct {
	HTTP string `json:"http,omitempty"`
	// Exec is a shell string or an argument list. The list runs without a
	// shell, which is the only form a scratch or distroless image can answer.
	Exec        any    `json:"exec,omitempty"`
	TCP         bool   `json:"tcp,omitempty"`
	Port        int    `json:"port,omitempty"`
	Interval    string `json:"interval,omitempty"`
	StartPeriod string `json:"start_period,omitempty"`
	Within      string `json:"within,omitempty"`
	Retries     int    `json:"retries,omitempty"`
}

type Drain struct {
	Signal string `json:"signal"`
	Wait   string `json:"wait,omitempty"`
	Grace  string `json:"grace,omitempty"`
}

type Logging struct {
	Driver  string         `json:"driver,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

type Resources struct {
	Memory string `json:"memory,omitempty"`
	CPUs   string `json:"cpus,omitempty"`
}

// Volume is a managed named volume or a bind mount. Exactly one form is set.
type Volume struct {
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	Mode   string `json:"mode"`
}

func (v Volume) IsBind() bool { return v.Source != "" }

type PublishedPort struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Bind      string `json:"bind"`
	Protocol  string `json:"protocol"`
}

type Persistence struct {
	Mode string `json:"mode"`
}

type Need struct {
	Name      string `json:"name"`
	Condition string `json:"condition"`
}

type Schedule struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
}

type Service struct {
	Driver      string         `json:"driver,omitempty"`
	Version     any            `json:"version"`
	Volumes     []string       `json:"volumes,omitempty"`
	Persistence *Persistence   `json:"persistence,omitempty"`
	Resources   *Resources     `json:"resources,omitempty"`
	Settings    map[string]any `json:"settings,omitempty"`
}

type Deployment struct {
	Order           []string `json:"order,omitempty"`
	RetainReleases  int      `json:"retain_releases"`
	MigrationPolicy string   `json:"migration_policy"`
}

type Runtime struct {
	EnvFiles  []string    `json:"env_files,omitempty"`
	Preflight []Preflight `json:"preflight,omitempty"`
}

type Preflight struct {
	File    string   `json:"file"`
	Require []string `json:"require,omitempty"`
	Present []string `json:"present,omitempty"`
}

type Command struct {
	Run   string `json:"run"`
	Local bool   `json:"local"`
}

type Verification struct {
	Workload           string            `json:"workload,omitempty"`
	HTTP               string            `json:"http,omitempty"`
	Exec               string            `json:"exec,omitempty"`
	Port               int               `json:"port,omitempty"`
	URL                string            `json:"url,omitempty"`
	StatusCodes        []int             `json:"status_codes,omitempty"`
	RequiredHeaders    map[string]string `json:"required_headers,omitempty"`
	Contains           string            `json:"contains,omitempty"`
	JSONAssertions     []JSONAssertion   `json:"json_assertions,omitempty"`
	MigrationRevisions *MigrationRevs    `json:"migration_revisions,omitempty"`
	Advisory           bool              `json:"advisory"`
}

type JSONAssertion struct {
	Path   string `json:"path"`
	Equals any    `json:"equals"`
}

type MigrationRevs struct {
	Job              string   `json:"job"`
	Provider         string   `json:"provider,omitempty"`
	AppliedRevisions []string `json:"applied_revisions"`
}

type Notification struct {
	Webhook string   `json:"webhook"`
	On      []string `json:"on,omitempty"`
	Format  string   `json:"format"`
}

type Registry struct {
	Server      string `json:"server"`
	Username    string `json:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
}

type Proxy struct {
	Managed      bool   `json:"managed"`
	Kind         string `json:"kind"`
	Image        string `json:"image,omitempty"`
	Config       string `json:"config,omitempty"`
	Network      string `json:"network"`
	CertResolver string `json:"cert_resolver,omitempty"`
}

type Secret struct {
	Provider string `json:"provider"`
	File     string `json:"file"`
}

type Observability struct {
	Logs    *LogSettings    `json:"logs,omitempty"`
	Metrics *MetricSettings `json:"metrics,omitempty"`
	Alerts  *AlertSettings  `json:"alerts,omitempty"`
}

type LogSettings struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days,omitempty"`
}

type MetricSettings struct {
	Enabled bool `json:"enabled"`
}

type AlertSettings struct {
	UnhealthyAfter string `json:"unhealthy_after,omitempty"`
}
