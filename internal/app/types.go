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
// check closedness against this model, decode, apply defaults, validate, then
// apply cross-field rules a field model cannot express. Expansion runs before
// validation because the rules describe the normalised form: `role` must be
// present before anything reasons about it.
package app

import "strings"

// Spec is the normalised form of a project file. Every field is concrete:
// defaults have been applied by the schema and shorthand has been expanded, so
// consumers never have to ask whether a value was stated or derived.
type Spec struct {
	// Dir is the directory holding the project file. Every repository path
	// resolves against it, so loading the same project from two working
	// directories yields the same runtime.
	Dir string `json:"-"`
	// file is the exact project path supplied to Load/LoadBytes. Mutating
	// operations such as eject must never reconstruct it as Dir/ob.yml.
	file string

	APIVersion string `json:"api_version" description:"Project contract version. Must be onebox.run/v1." example:"onebox.run/v1"`
	// Name is the application's name. Spelled Name rather than App because
	// inside a package called app, `spec.App` is a stutter and every caller
	// then writes `.App.App`. The authored key is still `app:`.
	Name             string                     `json:"app" description:"Stable application name used in generated container, volume, network, and host paths." example:"shop"`
	BasePath         string                     `json:"base_path" description:"Absolute host directory beneath which Onebox stores application state and releases." default:"/var/lib/ob" example:"/srv/ob"`
	Environments     map[string]Environment     `json:"environments" description:"Named environments, each naming the server it deploys to and the policy applied to it."`
	Workloads        map[string]Workload        `json:"workloads,omitempty" description:"Application containers, workers, daemons, and jobs managed as releases."`
	Services         map[string]Service         `json:"services,omitempty" description:"Supporting services managed outside application releases, such as databases and caches."`
	ExternalServices map[string]ExternalService `json:"external_services,omitempty" description:"Typed dependencies operated outside Onebox. Their connection projection is trusted, but their lifecycle and backup remain external."`
	BackupTargets    map[string]BackupTarget    `json:"backup_targets,omitempty" description:"User-owned off-host repositories available to service backup policies."`

	Deployment Deployment `json:"deployment" description:"Release ordering, retention, and migration behavior."`
	Runtime    *Runtime   `json:"runtime,omitempty" description:"Project-wide environment files and local environment-file requirements."`

	// envDefault is the selected environment's list, carried onto the resolved
	// clone so generation resolves without having to know which environment it
	// is rendering. Unexported: it is not a field of the contract, and the
	// closedness check reads the contract's fields from these tags.
	envDefault    []EnvFile
	Hooks         map[string]Command      `json:"hooks,omitempty" description:"Lifecycle commands keyed by seam: bootstrap, pre_release, post_release, or post_deploy."`
	Checks        Checks                  `json:"checks,omitzero" description:"Assertions that must pass before a release becomes current unless marked advisory."`
	Notifications map[string]Notification `json:"notifications,omitempty" description:"Named webhooks that receive selected operation outcomes."`
	Registries    map[string]Registry     `json:"registries,omitempty" description:"Named container registries and the environment variables holding their credentials."`
	Proxy         Proxy                   `json:"proxy" description:"Ownership and configuration of the host ingress proxy."`

	// rawExpanded is the authored input after shorthand expansion, kept so a
	// value's origin can be reported without threading a marker through every
	// field of the model.
	rawExpanded map[string]any

	// derivedPaths marks canonical paths the author did not write where they
	// now appear: moved by shorthand expansion, or injected by normalisation.
	derivedPaths map[string]Origin
}

type Environment struct {
	Server   Server `json:"server" description:"SSH server, written as user@host or as an object with host, user, and port." example:"root@203.0.113.10"`
	BasePath string `json:"base_path,omitempty" description:"Environment-specific replacement for the project base_path." example:"/srv/ob"`
	// EnvFiles is this environment's default list. It sits on the environment
	// rather than in an environment-scoped `runtime` block for the same reason
	// base_path does: an environment restating a project-level default is an
	// established shape here, and one field does not justify a second place
	// environments carry runtime settings.
	EnvFiles  []EnvFile  `json:"env_files,omitempty" description:"Default ordered environment-file list for application, worker, and job workloads in this environment."`
	Policy    Policy     `json:"policy" description:"Approval, runner compatibility, and migration-backup requirements for this environment."`
	Overrides *Overrides `json:"overrides,omitempty" description:"Environment-specific operational tuning. Overrides cannot change workload identity or data semantics."`
}

// Server is a scalar `user@host` or an object. Both decode here.
type Server struct {
	Host string `json:"host" description:"SSH hostname or IP address." example:"203.0.113.10"`
	User string `json:"user,omitempty" description:"SSH user. The local SSH configuration supplies it when omitted." example:"root"`
	Port int    `json:"port,omitempty" description:"SSH port. The SSH default is used when omitted." example:"2222"`
}

type Policy struct {
	RequireApproval     bool   `json:"require_approval" description:"Require a plan-bound local confirmation before mutating this environment." default:"true"`
	AllowAgentProposals bool   `json:"allow_agent_proposals" description:"Declared permission for agent-authored proposals. The current CLI does not distinguish agent identity; execution remains approval-gated." default:"true"`
	MinOneboxVersion    string `json:"min_onebox_version,omitempty" description:"Oldest released Onebox runner allowed to operate this environment." example:"v2026.8.0"`
	MinPlanSchema       string `json:"min_plan_schema,omitempty" description:"Oldest executable plan schema accepted by this environment." example:"onebox.run/executable-deploy-plan/v1alpha2"`
	// Migrations groups what this environment demands of a release that carries
	// migration risk. Grouped rather than four flat keys each repeating the
	// word: the prefix is the block's name now.
	Migrations MigrationPolicy `json:"migrations,omitzero" description:"What this environment requires of a release carrying migration risk."`
}

type MigrationPolicy struct {
	RequireBackup      bool     `json:"require_backup" description:"Require a plan-bound backup report before a release with migration risk." default:"false"`
	BackupMaxAge       string   `json:"backup_max_age,omitempty" description:"Maximum age of a backup report accepted for a migration." default:"24h" example:"24h"`
	RequireRestoreTest bool     `json:"require_restore_test" description:"Require the backup report to state that a restore test succeeded." default:"false"`
	BackupKeyMaterial  []string `json:"backup_key_material,omitempty" description:"Key-material identities the backup report must name." example:"BACKUP_ACCESS_KEY_ID"`
}

type Overrides struct {
	Workloads map[string]map[string]any `json:"workloads,omitempty" description:"Allowed workload tuning keyed by workload name: replicas, resources, env, env_files, strategy, and routes."`
	Services  map[string]map[string]any `json:"services,omitempty" description:"Allowed service tuning keyed by service name: resources and settings."`
}

// DataEffect is the closed data-impact vocabulary shared by authored jobs,
// executable plans, and the engine's post-lock execution check.
type DataEffect string

const (
	DataEffectNone        DataEffect = "none"
	DataEffectMigration   DataEffect = "migration"
	DataEffectDestructive DataEffect = "destructive"
	DataEffectUnknown     DataEffect = "unknown"
)

type Workload struct {
	Role     string `json:"role" description:"Lifecycle role: application, worker, daemon, or job." example:"application"`
	Build    *Build `json:"build,omitempty" description:"Build metadata for development. Production requires a resolved image supplied with --image."`
	Image    *Image `json:"image,omitempty" description:"Container image source, written as a reference string or an object." example:"ghcr.io/acme/shop:1.4.0"`
	Compose  string `json:"compose,omitempty" description:"Existing Compose service to adopt, as repository path#service." example:"docker-compose.yml#web"`
	Command  any    `json:"command,omitempty" description:"Container command as a shell string or argument list." example:"./bin/server"`
	Replicas int    `json:"replicas" description:"Desired number of long-running workload containers." default:"1" example:"2"`
	Strategy string `json:"strategy,omitempty" description:"Release strategy. Defaults to rolling only for an application workload with health; all other workloads default to recreate."`

	Domain string  `json:"domain,omitempty" description:"Domain shorthand for one HTTPS route; requires port and cannot be combined with routes." example:"shop.example.com"`
	Port   int     `json:"port,omitempty" description:"Container port used with domain shorthand and as the default HTTP health port." example:"3000"`
	Routes []Route `json:"routes,omitempty" description:"Ingress routes exposed by this workload."`

	Health         *Health         `json:"health,omitempty" description:"Readiness check used to gate rolling replacement."`
	Drain          *Drain          `json:"drain,omitempty" description:"Signal and timing used to remove a container from traffic before stopping it."`
	Resources      *Resources      `json:"resources,omitempty" description:"Container memory and CPU limits."`
	Env            map[string]any  `json:"env,omitempty" description:"Literal container environment values. Managed-service credential variables cannot be overridden."`
	EnvFiles       []EnvFile       `json:"env_files,omitempty" description:"Workload-specific ordered environment-file list. Replaces broader defaults when present."`
	Volumes        []Volume        `json:"volumes,omitempty" description:"Managed named volumes or repository bind mounts."`
	PublishedPorts []PublishedPort `json:"published_ports,omitempty" description:"Host ports published outside the proxy. They bind to loopback by default. A rolling workload cannot publish one, because two replicas cannot hold the same host port during a roll: set strategy: recreate, or route through the proxy instead."`
	Persistence    *Persistence    `json:"persistence,omitempty" description:"Declares whether this workload holds data that must outlive releases."`
	Needs          []Need          `json:"needs,omitempty" description:"Workload or supporting-service prerequisites and optional connection-variable mappings."`

	Entrypoint any            `json:"entrypoint,omitempty" description:"Container entrypoint as a string or argument list."`
	User       string         `json:"user,omitempty" description:"User or UID used to run the container process."`
	Hostname   string         `json:"hostname,omitempty" description:"Hostname assigned inside the workload container."`
	WorkingDir string         `json:"working_dir,omitempty" description:"Absolute working directory for the container process." example:"/app"`
	Init       *bool          `json:"init,omitempty" description:"Run a minimal init process as PID 1 inside the container."`
	TTY        *bool          `json:"tty,omitempty" description:"Allocate a pseudo-TTY for the container."`
	StdinOpen  *bool          `json:"stdin_open,omitempty" description:"Keep standard input open for the container."`
	ExtraHosts []string       `json:"extra_hosts,omitempty" description:"Additional host-to-address entries added to the container."`
	Labels     map[string]any `json:"labels,omitempty" description:"Additional container labels outside namespaces reserved by Onebox and the proxy."`
	Logging    *Logging       `json:"logging,omitempty" description:"Container logging driver and driver-specific options."`

	// Job only.
	When       string     `json:"when,omitempty" description:"When a job runs: manual, pre_release, or post_release." default:"manual"`
	DataEffect DataEffect `json:"data_effect,omitempty" description:"Job data impact used by rollback and abort gates." example:"migration"`
	Schedule   *Schedule  `json:"schedule,omitempty" description:"Host-resident recurring schedule for a job."`
}

type Build struct {
	Context    string         `json:"context" description:"Repository-relative build context." example:"."`
	Dockerfile string         `json:"dockerfile,omitempty" description:"Repository-relative Dockerfile path." example:"Dockerfile"`
	Target     string         `json:"target,omitempty" description:"Named Dockerfile stage to build."`
	Args       map[string]any `json:"args,omitempty" description:"Build arguments supplied by the external build system."`
}

type Image struct {
	Reference string `json:"reference" description:"Complete container image reference, optionally tagged or digest-pinned." example:"ghcr.io/acme/shop:1.4.0"`
	Pull      string `json:"pull" description:"When to fetch the image from the registry: missing fetches only what the host does not already hold, always fetches every release, never fetches at all and fails on a missing image." default:"missing"`
}

type Route struct {
	Domain      string          `json:"domain" description:"DNS name matched by the proxy." example:"shop.example.com"`
	Path        string          `json:"path" description:"URL path prefix matched by an HTTP route." default:"/"`
	Port        int             `json:"port" description:"Container port receiving routed traffic." example:"3000"`
	Entrypoint  string          `json:"entrypoint" description:"Named proxy listener used for the route." default:"websecure"`
	Protocol    string          `json:"protocol" description:"Routing protocol: http, tcp, or udp." default:"http"`
	Scheme      string          `json:"scheme" description:"Backend connection scheme: http, https, h2c, tcp, or udp." default:"http"`
	TLS         string          `json:"tls" description:"TLS handling: terminate, passthrough, or none." default:"terminate"`
	Middlewares []MiddlewareRef `json:"middlewares,omitempty" description:"Ordered provider-qualified middleware references applied to this route."`
}

// MiddlewareRef names dynamic proxy configuration without opening the
// traefik.* label namespace to authored input. The provider suffix is required
// because Onebox-generated routers live in the Docker provider while project
// middleware commonly lives in the file provider.
type MiddlewareRef string

type Health struct {
	HTTP string `json:"http,omitempty" description:"HTTP path probed inside the container." example:"/healthz"`
	// Exec is a shell string or an argument list. The list runs without a
	// shell, which is the only form a scratch or distroless image can answer.
	Exec        any    `json:"exec,omitempty" description:"Health command as a shell string or direct argument list."`
	TCP         bool   `json:"tcp,omitempty" description:"Probe the configured port by opening a TCP connection." default:"false"`
	Port        int    `json:"port,omitempty" description:"Container port probed by HTTP or TCP health checks." example:"8080"`
	Interval    string `json:"interval,omitempty" description:"Delay between container health probes." example:"2s"`
	StartPeriod string `json:"start_period,omitempty" description:"Startup grace period before failed probes count." example:"5s"`
	Within      string `json:"within,omitempty" description:"Maximum time a rollout waits for readiness." example:"120s"`
	Retries     int    `json:"retries,omitempty" description:"Consecutive failed probes before the container is unhealthy." example:"3"`
}

type Drain struct {
	Signal string `json:"signal" description:"Signal sent to begin graceful shutdown." default:"TERM"`
	Wait   string `json:"wait,omitempty" description:"Time allowed for the proxy to stop routing before shutdown begins." example:"10s"`
	Grace  string `json:"grace,omitempty" description:"Maximum graceful-shutdown time before forced termination." example:"30s"`
}

type Logging struct {
	Driver  string         `json:"driver,omitempty" description:"Container runtime logging driver." example:"local"`
	Options map[string]any `json:"options,omitempty" description:"Driver-specific logging options passed to the container runtime."`
}

type Resources struct {
	Memory string `json:"memory,omitempty" description:"Container memory limit." example:"512MB"`
	CPUs   string `json:"cpus,omitempty" description:"Container CPU limit expressed as a positive decimal count." example:"0.5"`
}

// Volume is a managed named volume or a bind mount. Exactly one form is set.
type Volume struct {
	Name   string `json:"name,omitempty" description:"Stable logical name of a Onebox-managed volume." example:"data"`
	Path   string `json:"path,omitempty" description:"Absolute container path where the volume or bind mount is attached." example:"/var/lib/app"`
	Source string `json:"source,omitempty" description:"Repository-relative source path of a bind mount." example:"./config"`

	Mode string `json:"mode" description:"Mount access mode: rw or ro." default:"rw"`
}

func (v Volume) IsBind() bool { return v.Source != "" }

type PublishedPort struct {
	Host      int    `json:"host" description:"Port exposed on the host." example:"8080"`
	Container int    `json:"container" description:"Port receiving traffic inside the container." example:"3000"`
	Bind      string `json:"bind" description:"Host address on which the published port listens." default:"127.0.0.1"`
	Protocol  string `json:"protocol" description:"Published transport protocol: tcp or udp." default:"tcp"`
}

type Persistence struct {
	Mode string `json:"mode" description:"Data lifetime: durable, ephemeral, or external." default:"durable"`
}

type Need struct {
	Name      string `json:"name" description:"Name of a workload or supporting service that must start first."`
	Condition string `json:"condition" description:"Prerequisite condition: started, healthy, or completed."`
	// Env maps a variable the workload reads to the part of the connection
	// that belongs in it.
	Env map[string]string `json:"env,omitempty" description:"Maps application environment-variable names to service connection parts such as host, port, user, password, database, or url."`
}

type Schedule struct {
	Cron     string `json:"cron" description:"Five-field cron schedule translated to a host timer." example:"0 2 * * *"`
	Timezone string `json:"timezone" description:"IANA timezone used to interpret the cron schedule." default:"UTC" example:"Europe/Berlin"`
}

type Service struct {
	Driver      string         `json:"driver,omitempty" description:"Built-in service driver. Defaults to the service map key." example:"postgres"`
	Version     any            `json:"version" description:"Driver version or image tag to run." example:"17"`
	Volumes     []string       `json:"volumes,omitempty" description:"Additional driver-defined persistent volume names."`
	Persistence *Persistence   `json:"persistence,omitempty" description:"Data-lifetime declaration for this supporting service."`
	Resources   *Resources     `json:"resources,omitempty" description:"Memory and CPU limits for this supporting service."`
	Settings    map[string]any `json:"settings,omitempty" description:"Driver-specific settings validated by the selected service driver."`
	Backup      *BackupPolicy  `json:"backup,omitempty" description:"Recovery intent for this service. Onebox selects the qualified native implementation; declaring intent alone does not establish backup."`
}

// BackupTarget is a closed S3-compatible destination declaration. It accepts
// credential references only, never inline values.
type BackupTarget struct {
	Kind          string              `json:"kind" description:"Destination kind. Only s3-compatible is supported." example:"s3-compatible"`
	Endpoint      string              `json:"endpoint" description:"Destination API endpoint. HTTPS is required unless tls is explicitly insecure." example:"https://objects.example.com"`
	Bucket        string              `json:"bucket,omitempty" description:"Existing destination bucket used by this target." example:"onebox-backups"`
	Prefix        string              `json:"prefix,omitempty" description:"Non-secret object prefix reserved for Onebox backup data." example:"production/shop"`
	Region        string              `json:"region,omitempty" description:"S3-compatible region when the endpoint requires one." example:"us-east-1"`
	TLS           string              `json:"tls" description:"Transport policy: verify, or skip-verify to accept a plaintext http endpoint." default:"verify"`
	FailureDomain FailureDomain       `json:"failure_domain" description:"Operator-declared identity used to prove the destination does not share the protected host."`
	Credentials   CredentialReference `json:"credentials" description:"Trusted encrypted-file entries containing destination credentials; values never appear in the project."`
	Encryption    TargetEncryption    `json:"encryption" description:"Required encryption mode for each recovery kind this target may store."`
}

type FailureDomain struct {
	Identity string `json:"identity" description:"Stable operator-owned failure-domain identity, distinct from the protected host." example:"provider-a/us-east-1/account-42"`
	Host     string `json:"host,omitempty" description:"Destination host identity used to refuse a target on the protected host." example:"backup-01.example.net"`
}

// CredentialReference names entries in a trusted SOPS file. Entry names are
// ordinary environment-variable identifiers; credential values are never
// accepted by this model.
type CredentialReference struct {
	File              string `json:"file" description:"Repository-relative encrypted credential file staged through the trusted secret flow." example:"secrets/backup.env"`
	Provider          string `json:"provider" description:"Trusted secret provider. Only sops is currently executable." default:"sops"`
	AccessKeyEntry    string `json:"access_key_entry" description:"Variable name containing the destination access key." example:"BACKUP_ACCESS_KEY_ID"`
	SecretKeyEntry    string `json:"secret_key_entry" description:"Variable name containing the destination secret key." example:"BACKUP_SECRET_ACCESS_KEY"`
	SessionTokenEntry string `json:"session_token_entry,omitempty" description:"Optional variable name containing a temporary destination session token." example:"BACKUP_SESSION_TOKEN"`
}

type TargetEncryption struct {
	Snapshot string `json:"snapshot,omitempty" description:"Encryption mode required for snapshot recovery: client-side or server-side."`
	PITR     string `json:"pitr,omitempty" description:"Encryption mode required for point-in-time recovery: client-side or server-side."`
	Cold     string `json:"cold,omitempty" description:"Encryption mode required for cold recovery: client-side or server-side."`
}

type BackupPolicy struct {
	Target        string          `json:"target" description:"Name of a project-level backup target." example:"offsite"`
	RecoveryKind  string          `json:"recovery_kind" description:"Required recovery envelope: snapshot, pitr, or cold." example:"pitr"`
	MaxDataLoss   string          `json:"max_data_loss" description:"Maximum tolerable interval between the latest recoverable point and failure." example:"15m"`
	AllowDowntime bool            `json:"allow_downtime" description:"Whether recurring backup operations may use the driver-declared stopped-service window." default:"false"`
	Schedule      Schedule        `json:"schedule" description:"Exact recurring base-backup schedule."`
	Retention     BackupRetention `json:"retention" description:"Portable minimum recovery history that the selected native driver must be able to preserve."`
	Drill         BackupDrill     `json:"drill" description:"Exact isolated restore-test schedule, proof age, and optional staging filesystem."`
}

type BackupRetention struct {
	Keep   int    `json:"keep" description:"Minimum number of independently recoverable base generations to retain." default:"7" example:"7"`
	Window string `json:"window" description:"Minimum continuous recovery history the native retention mapping must preserve." default:"7d" example:"7d"`
}

type BackupDrill struct {
	Schedule Schedule `json:"schedule" description:"Exact recurring isolated restore-test schedule."`
	MaxAge   string   `json:"max_age" description:"Maximum age of the latest passing restore proof." default:"7d" example:"7d"`
}

type ExternalService struct {
	Driver      string                 `json:"driver" description:"Built-in connection shape used to validate and project this dependency." example:"postgres"`
	Connection  ExternalConnection     `json:"connection" description:"Trusted connection source and driver-shaped entry mapping."`
	BackupOwner string                 `json:"backup_owner" description:"Operator or provider responsible for backup, restore, upgrades, credentials, and durability." example:"platform-team/rds"`
	Probe       *ExternalReadOnlyProbe `json:"probe,omitempty" description:"Optional bounded read-only health observation; it never creates or repairs provider resources."`
}

type ExternalConnection struct {
	Source  ExternalConnectionSource `json:"source" description:"Trusted encrypted file containing the connection values."`
	Entries map[string]string        `json:"entries" description:"Maps driver connection parts such as host, port, user, password, database, or url to variable names in the trusted source."`
}

type ExternalConnectionSource struct {
	File     string `json:"file" description:"Repository-relative encrypted environment file staged through the trusted secret flow." example:"secrets/production-db.env"`
	Provider string `json:"provider" description:"Trusted secret provider. Only sops is currently executable." default:"sops"`
}

type ExternalReadOnlyProbe struct {
	Kind    string `json:"kind" description:"Read-only observation kind: driver-health." default:"driver-health"`
	Timeout string `json:"timeout" description:"Maximum duration of one read-only probe." default:"5s" example:"5s"`
	MaxAge  string `json:"max_age" description:"Maximum age of a probe observation bound into a plan." default:"5m" example:"5m"`
}

type Deployment struct {
	Order           []string `json:"order,omitempty" description:"Explicit workload release order. Dependency order is derived when omitted."`
	RetainReleases  int      `json:"retain_releases" description:"Number of completed release directories retained for inspection and rollback." default:"5"`
	MigrationPolicy string   `json:"migration_policy" description:"Policy for migration jobs during release and recovery." default:"manual"`
}

type Runtime struct {
	EnvFiles  []EnvFile  `json:"env_files,omitempty" description:"Project-wide ordered environment-file list for application, worker, and job workloads."`
	EnvChecks []EnvCheck `json:"env_checks,omitempty" description:"Local environment-file assertions checked before planning or deploying."`
}

type EnvCheck struct {
	File    string   `json:"file" description:"Repository-relative dotenv file whose declared keys are checked." example:".env.production"`
	Require []string `json:"require,omitempty" description:"Environment keys that must be declared with non-empty values."`
	Present []string `json:"present,omitempty" description:"Environment keys that must be declared but may be empty."`
}

type Command struct {
	Run   string `json:"run" description:"Command executed at the lifecycle seam." example:"./scripts/notify.sh"`
	Local bool   `json:"local" description:"Run on the operator machine instead of the server." default:"false"`
}

// Checks are the post-release assertions, grouped by kind.
//
// Grouped rather than one list of four shapes. The flat form was an untagged
// union — `status_codes`, `required_headers`, `contains`, and `json_assertions`
// were legal only alongside `url`, and nothing said so until validation. A group
// per kind lets the schema type each one, so an editor can complete it and a
// wrong field is a wrong field rather than a runtime refusal.
type Checks struct {
	HTTP       []HTTPCheck      `json:"http,omitempty" description:"HTTP paths probed inside a named workload."`
	URL        []URLCheck       `json:"url,omitempty" description:"External URLs probed from the operator side."`
	Exec       []ExecCheck      `json:"exec,omitempty" description:"Commands run inside a named workload."`
	Migrations []MigrationCheck `json:"migrations,omitempty" description:"Migration revisions checked against captured job evidence."`
}

type HTTPCheck struct {
	Workload string `json:"workload" description:"Workload the path is probed inside." example:"web"`
	Path     string `json:"path" description:"HTTP path verified inside the workload." example:"/healthz"`
	Port     int    `json:"port,omitempty" description:"Container port to probe." example:"3000"`
	Advisory bool   `json:"advisory,omitempty" description:"Report a failure without blocking release activation." default:"false"`
}

type URLCheck struct {
	URL             string            `json:"url" description:"External HTTP or HTTPS URL verified from the operator side." example:"https://shop.example.com/healthz"`
	StatusCodes     []int             `json:"status_codes,omitempty" description:"Allowed response status codes. A successful 2xx response is expected when omitted."`
	RequiredHeaders map[string]string `json:"required_headers,omitempty" description:"Exact response headers required for success."`
	Contains        string            `json:"contains,omitempty" description:"Text the response body must contain."`
	JSONAssertions  []JSONAssertion   `json:"json_assertions,omitempty" description:"Scalar JSON response values that must match exactly."`
	Advisory        bool              `json:"advisory,omitempty" description:"Report a failure without blocking release activation." default:"false"`
}

type ExecCheck struct {
	Workload string `json:"workload" description:"Workload the command runs inside." example:"web"`
	Run      string `json:"run" description:"Shell command verified inside the workload." example:"test -f /srv/ready"`
	Advisory bool   `json:"advisory,omitempty" description:"Report a failure without blocking release activation." default:"false"`
}

type MigrationCheck struct {
	Job              string   `json:"job" description:"Job workload whose captured evidence is checked." example:"migrate"`
	Provider         string   `json:"provider" description:"Migration tool that produced the revisions." example:"alembic"`
	AppliedRevisions []string `json:"applied_revisions" description:"Revisions the job must report as applied."`
	Advisory         bool     `json:"advisory,omitempty" description:"Report a failure without blocking release activation." default:"false"`
}

// RunnableCheck is the flat form every check is executed as. Authors never
// write it; Checks.All produces it, so the runner keeps one shape to run.
type RunnableCheck struct {
	Workload           string
	HTTP               string
	Exec               string
	Port               int
	URL                string
	StatusCodes        []int
	RequiredHeaders    map[string]string
	Contains           string
	JSONAssertions     []JSONAssertion
	MigrationRevisions *MigrationRevs
	Advisory           bool
}

// All flattens the grouped checks into the order they are declared in: every
// HTTP check, then URL, then exec, then migrations.
func (c Checks) All() []RunnableCheck {
	out := make([]RunnableCheck, 0, len(c.HTTP)+len(c.URL)+len(c.Exec)+len(c.Migrations))
	for _, check := range c.HTTP {
		out = append(out, RunnableCheck{Workload: check.Workload, HTTP: check.Path, Port: check.Port, Advisory: check.Advisory})
	}
	for _, check := range c.URL {
		out = append(out, RunnableCheck{
			URL: check.URL, StatusCodes: check.StatusCodes, RequiredHeaders: check.RequiredHeaders,
			Contains: check.Contains, JSONAssertions: check.JSONAssertions, Advisory: check.Advisory,
		})
	}
	for _, check := range c.Exec {
		out = append(out, RunnableCheck{Workload: check.Workload, Exec: check.Run, Advisory: check.Advisory})
	}
	for _, check := range c.Migrations {
		revisions := MigrationRevs{Job: check.Job, Provider: check.Provider, AppliedRevisions: check.AppliedRevisions}
		out = append(out, RunnableCheck{MigrationRevisions: &revisions, Advisory: check.Advisory})
	}
	return out
}

type JSONAssertion struct {
	Path   string `json:"path" description:"Dot-separated path to a scalar value in the JSON response." example:"service.ready"`
	Equals any    `json:"equals" description:"Exact scalar value required at path."`
}

type MigrationRevs struct {
	Job              string   `json:"job" description:"Migration job whose result evidence is checked." example:"migrate"`
	Provider         string   `json:"provider,omitempty" description:"Migration provider expected in the job result." example:"atlas"`
	AppliedRevisions []string `json:"applied_revisions" description:"Ordered migration revisions expected to be applied."`
}

type Notification struct {
	Webhook string   `json:"webhook" description:"HTTP endpoint that receives operation notifications." example:"https://hooks.example.com/onebox"`
	On      []string `json:"on,omitempty" description:"Operation outcomes that trigger this notification." default:"success, failure"`
	Format  string   `json:"format" description:"Notification payload format." default:"text"`
}

type Registry struct {
	Server      string `json:"server" description:"Registry hostname, optionally with a port." example:"ghcr.io"`
	Username    string `json:"username,omitempty" description:"Registry login username."`
	PasswordEnv string `json:"password_env,omitempty" description:"Local environment-variable name containing the registry password or token." example:"GHCR_TOKEN"`
}

type Proxy struct {
	Managed      bool   `json:"managed" description:"Let Onebox converge the host-scoped proxy when routes are declared."`
	Kind         string `json:"kind" description:"Proxy implementation, or none to disable routing." default:"traefik-docker"`
	Image        string `json:"image,omitempty" description:"Container image used for the managed proxy."`
	Config       string `json:"config,omitempty" description:"Repository-relative static proxy configuration directory owned by the project; it must contain exactly one of traefik.yml or traefik.yaml."`
	Network      string `json:"network" description:"External container network shared with routed workloads." default:"ob-ingress"`
	CertResolver string `json:"cert_resolver,omitempty" description:"Traefik certificate resolver used by terminating TLS routes."`
}

// EnvFile is one contributor of environment values.
//
// A plaintext file and an encrypted one differ in how the bytes are obtained
// and in nothing else — not in who receives them, not in how they compose, not
// in where they may be declared. They were two mechanisms with two sets of
// rules, and the rules disagreed. One entry type, and `provider` says how to
// read it.
type EnvFile struct {
	File     string `json:"file" description:"Repository-relative environment file path." example:".env.production"`
	Provider string `json:"provider,omitempty" description:"Decryptor used before staging the file. The supported encrypted provider is sops." example:"sops"`
}

// Encrypted reports whether the entry needs decrypting before it can be read.
func (e EnvFile) Encrypted() bool { return e.Provider != "" }

// StagedPath is where the container runtime reads the entry from, relative to
// the release directory the generated document sits in.
//
// A plaintext entry is staged at its own repository path, so the generated
// runtime names what the author wrote. An encrypted entry is decrypted into a
// file beside it, named from the entry rather than shared, because two
// encrypted entries in one list are two files and a single shared name would
// silently make the later one win outright instead of key by key.
func (e EnvFile) StagedPath() string {
	if !e.Encrypted() {
		return e.File
	}
	// The prefix is what separates a staged name from an authored path. A
	// plaintext entry keeps its own path, so without a prefix no authored path
	// could ever collide with one — but an authored path may itself begin with
	// the prefix, so the provider is folded in as well: two entries naming one
	// file under different providers are different entries and must not share
	// a file.
	// Injective: the replacement character is escaped before the separator is
	// replaced, so `a/s.env` and `a-s.env` derive different names. Replacing
	// the separator alone collides, and the generated document would then list
	// one name twice and quietly keep whichever entry came last.
	escaped := strings.ReplaceAll(strings.ReplaceAll(e.File, "-", "--"), "/", "-")
	return ".ob-decrypted-" + e.Provider + "-" + escaped
}
