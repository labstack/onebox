// Normative shape for the onebox.run/v1 authoring contract.
//
// Part of the specification, not the implementation. Prose cannot state
// requiredness, exclusivity, bounds, and closure precisely enough for two
// implementations to accept the same corpus; this can, and it compiles.
//
// Two CUE rules govern the style here, both learned from review:
//
//   - A default only materialises on a REGULAR field. `a?: int | *1` leaves `a`
//     absent when omitted; `a: int | *1` yields 1. Every field carrying a
//     documented default is therefore regular, not optional.
//   - Cardinality validators (struct.MinFields, list.MinItems) evaluate eagerly
//     and fail against the bare definition, which would break the loader's own
//     schema check. Non-emptiness — at least one environment, a non-empty
//     workloads block, non-empty assertion lists — is therefore enforced by the
//     loader and stated in the specification, not here.
//   - This schema describes the NORMALISED project: the loader expands shorthand
//     and fills documented defaults first, then validates. A discriminator left
//     to a default keeps every branch of a disjunction alive — a workload with
//     no `role` satisfies the worker branch as readily as the application one —
//     so discriminators are required here and supplied by normalisation.
//   - Closure is not discrimination. A disjunction that merely anchors one key
//     still admits every other key on every branch, so variants are written as
//     separate closed structs.
//
// Path kinds are distinguished: #RepoPath resolves against the project file and
// stays inside the repository, #AbsPath is a path on the target, #UrlPath is a
// request path.
package obschema

// ---------- scalars ----------

#Ident:    =~"^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$"
#AppIdent: #Ident & !~"^ob-" & !="ob" & !="proxy" & !="_host"

#Port:       int & >0 & <65536
#StatusCode: int & >=100 & <=599
#PosInt:     int & >0
#Days:       int & >0

// Compound Go durations plus the whole-day form the previous contract accepted.
#Dur: =~"^(([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+|[0-9]+d)$"

#Cron:   =~"^[-0-9*/,A-Za-z ]+$"
#TZ:     string & !=""
#Signal: =~"^[A-Z][A-Z0-9]*$"
#Size:   =~"^[0-9]+(\\.[0-9]+)?(B|KB|MB|GB|TB)$"
#Cpus:   =~"^[0-9]+(\\.[0-9]+)?$"

// Repository-relative. Absolute is refused lexically; escaping the repository
// is a semantic check the loader performs after resolution, because `a/../b`
// is legal and resolves inside.
#RepoPath: string & !="" & !~"^/"

#AbsPath: =~"^/"
#UrlPath: =~"^/"

#ImageRef: string & !=""
#EnvName:  =~"^[A-Za-z_][A-Za-z0-9_]*$"
#Scalar:   string | int | float | bool

#PlanSchema: =~"^onebox\\.run/executable-deploy-plan/v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$"
#CalVer:     =~"^v[0-9]{4}\\.(0[1-9]|1[0-2])\\.[1-9][0-9]*$"

// Extension keys, accepted and ignored wherever a mapping is accepted.
#X: {[=~"^x-"]: _}

// ---------- sources ----------

#Build: #RepoPath | {
	context:     #RepoPath
	dockerfile?: #RepoPath
	target?:     string & !=""
	args?: [#EnvName]: #Scalar
	platform?: string & !=""
	#X
}

#Image: #ImageRef | {
	reference: #ImageRef
	platform?: string & !=""
	pull:      "always" | "missing" | "never" | *"missing"
	registry?: #Ident
	#X
}

// file#service. The file part is a repository path, so it may not be absolute.
#ComposeRef: =~"^[^/#][^#]*#[a-zA-Z0-9._-]+$"

// ---------- health ----------

#HealthHTTP: {http: #UrlPath, port?: #Port, exec?: _|_, tcp?: _|_, #HealthTiming, #X}
// A string check runs through a shell; a list is executed directly. The list
// form exists because an image built from scratch or distroless has no shell,
// and a health check it cannot run is a workload that can never be released.
#HealthExec: {exec: string & !="" | [string & !="", ...string], http?: _|_, tcp?: _|_, port?: _|_, #HealthTiming, #X}
#HealthTCP:  {tcp: true, port: #Port, http?: _|_, exec?: _|_, #HealthTiming, #X}

#HealthTiming: {
	...
	interval?:     #Dur
	start_period?: #Dur
	within?:       #Dur
	retries?:      #PosInt
}

#Health: #UrlPath | #HealthHTTP | #HealthExec | #HealthTCP

// ---------- routing ----------

// entrypoint names the proxy listener; port is the container port behind it.
// Separating them is what makes a non-HTTP listener expressible.
#Route: {
	domain:     string & !=""
	path:       #UrlPath | *"/"
	port:       #Port
	entrypoint: string & !="" | *"websecure"
	protocol:   "http" | "tcp" | *"http"
	// How the proxy speaks to the workload behind it. gRPC backends need h2c,
	// which is not expressible by protocol and TLS mode alone.
	scheme: "http" | "https" | "h2c" | *"http"
	tls:    "terminate" | "passthrough" | "none" | *"terminate"
	#X
}

// ---------- storage ----------

// Either an Onebox-managed named volume, or a bind mount. Bind mounts are not
// exotic: four of the external projects converted against this contract use one
// for their database directory or their configuration, and so do two here.
// The branches are mutually negated because a branch missing a required field
// is incomplete rather than invalid and would never drop out.
#Volume: #Ident | {
	name:    #Ident
	path?:   #AbsPath
	mode:    "rw" | "ro" | *"rw"
	source?: _|_
	target?: _|_
	#X
} | {
	source: #RepoPath | #AbsPath
	target: #AbsPath
	mode:   "rw" | "ro" | *"rw"
	name?:  _|_
	path?:  _|_
	#X
}

#Persistence: {
	mode: "durable" | "ephemeral" | "external" | *"durable"
	#X
}

#Resources: {memory?: #Size, cpus?: #Cpus, #X}

// A prerequisite. Ten real projects were converted to check this contract and
// every one of them used a health-gated dependency, so the condition defaults to
// `healthy` rather than to mere start order.
#Need: #Ident | {
	name: #Ident
	// Deliberately undefaulted here. The loader resolves it: healthy when the
	// dependency declares a health check, started when it does not. Defaulting
	// to healthy in the schema produced a runtime the container engine refuses
	// to start, because a dependency without a health check can never become
	// healthy.
	condition?: "started" | "healthy" | "completed"
	#X
}

// A published host port, for a workload that is reached without the proxy.
// The bind address defaults to loopback: publishing to every interface is a
// deliberate act, not a typo.
#PublishedPort: {
	host:      #Port
	container: #Port
	bind:      string & !="" | *"127.0.0.1"
	protocol:  "tcp" | "udp" | *"tcp"
	#X
}

// ---------- protection ----------

#Schedule: {
	cron:     #Cron
	timezone: #TZ | *"UTC"
	#X
}

#Backup: {
	schedule?:       #Schedule
	retention_days?: #Days
	restore_drill?:  {schedule: #Schedule, #X}
	destination?:    string & !=""
	#X
}

#Protection: {
	backup?:        #Backup
	restore_drill?: {schedule: #Schedule, #X}
	#X
}

// A command is an argument list, a bare string, or the hook form.
#Command: string & !="" | [...string] | {
	run:   string & !=""
	local: bool | *false
	#X
}

// ---------- workloads ----------

// Exclusivity is written as explicit negation on open structs. A closed struct
// cannot be embedded into another closed struct without collapsing the
// disjunction, and an open struct alone would let a second source slip in.
#Source: {build: #Build, image?: _|_, compose?: _|_, ...} |
	{image: #Image, build?: _|_, compose?: _|_, ...} |
	{compose: #ComposeRef, build?: _|_, image?: _|_, ...}

#WorkloadCommon: {
	...
	command?:  #Command
	replicas:  #PosInt | *1
	health?:   #Health
	drain?:    {signal: #Signal | *"TERM", wait?: #Dur, grace?: #Dur, #X}
	resources?: #Resources
	env?: {[#EnvName]: #Scalar}
	volumes?: [...#Volume]
	persistence?: #Persistence
	protection?:  #Protection
	needs?: [...#Need]
	ports?: [...#PublishedPort]

	// Passthrough fields. Each is a scalar, a bool or a flat list carrying no
	// new concept, and together they are what a survey of 276 real projects
	// showed standing between two thirds of services and the declaration.
	// Labels reserve the two namespaces Onebox generates into.
	entrypoint?:  string | [...string]
	user?:        string & !=""
	hostname?:    string & !=""
	working_dir?: #AbsPath
	init?:        bool
	tty?:         bool
	stdin_open?:  bool
	extra_hosts?: [...string & !=""]
	labels?: {[!~"^(ob\\.|traefik\\.)"]: #Scalar}

	// Onebox owns log rotation, but which driver a workload logs through is a
	// real choice people make — it blocked more services than anything else in
	// the survey — so the driver is declared and rotation stays Onebox's.
	logging?: {
		driver?: string & !=""
		options?: {[string]: #Scalar}
		#X
	}

	// Files projected into this workload, applied in order, later winning.
	// Per-workload because a real stack has several: paperless gives one file to
	// its web server only, immich shares one between two of four services, and
	// fanout has three for three services. A project-wide list would leak every
	// secret into every container.
	env_files?: [...#RepoPath]
	#X
}

// Routing fields are optional here and their exclusivity — the scalar pair or
// the list, never both, and the pair always together — is enforced by the
// loader. Expressing it as a disjunction with an empty branch leaves the union
// unresolvable, because a branch missing a required field is incomplete rather
// than invalid and never drops out.
#Routing: {
	...
	domain?: string & !=""
	port?:   #Port
	routes?: [...#Route]
}

// Job-only fields are explicitly negated on the other roles: #WorkloadCommon is
// open so embedding works, and an open struct would otherwise admit them.
// Roles are separate structs so job-only fields cannot appear elsewhere,
// and so a job cannot omit its data effect.
#WorkloadApplication: {
	role:      "application"
	run?:         _|_
	data_effect?: _|_
	schedule?:    _|_
	strategy:  "rolling" | "recreate" | *"rolling"
	#Source
	#Routing
	#WorkloadCommon
}

#WorkloadWorker: {
	role:     "worker"
	run?:         _|_
	data_effect?: _|_
	schedule?:    _|_
	strategy: "rolling" | "recreate" | *"recreate"
	#Source
	#WorkloadCommon
}

// A long-running supporting container the user owns: a database they still
// author, a cache, a cron runner, a scanner. Distinguished from application and
// worker because environment files are not projected into it and it never
// receives ingress unless it declares a route.
#WorkloadDaemon: {
	role:     "daemon"
	run?:         _|_
	data_effect?: _|_
	schedule?:    _|_
	strategy: "recreate" | *"recreate"
	#Source
	#Routing
	#WorkloadCommon
}

// A job runs to completion: at a release phase, on a schedule, or on demand.
#WorkloadJob: {
	role:        "job"
	run:         "pre_release" | "post_release" | "manual" | *"manual"
	data_effect: "none" | "migration" | "destructive" | "unknown"
	schedule?:   #Schedule
	#Source
	#WorkloadCommon
}

#Workload: #WorkloadApplication | #WorkloadWorker | #WorkloadDaemon | #WorkloadJob

// ---------- services ----------

// A service's name is its driver unless `driver` says otherwise, which is what
// makes `services: {postgres: 17}` sufficient. A second instance of the same
// driver names it explicitly: `events: {driver: postgres, version: 17}`. The
// closed set of drivers is enforced by the loader — CUE cannot hold it without
// duplicating the catalogue in two places that would drift.
#Service: string | int | {
	driver?:  #Ident
	version:  string | int
	volumes?: [...#Ident]
	persistence?: #Persistence
	resources?:   #Resources
	settings?: {[string]: #Scalar}
	backup?: #Backup
	#X
}

// ---------- environments ----------

#Server: string & !="" | {
	host:  string & !=""
	user?: string & !=""
	port?: #Port
	#X
}

#Policy: {
	require_approval:               bool | *true
	allow_agent_proposals:          bool | *true
	minimum_onebox_version?:        #CalVer
	minimum_plan_schema?:           #PlanSchema
	require_migration_backup:       bool | *false
	migration_backup_max_age?:      #Dur
	require_migration_restore_test: bool | *false
	migration_backup_key_material?: [...string]
	#X
}

// Null removes a key. Nested maps admit null members so a single setting can be
// removed without replacing the whole object.
#Overrides: {
	workloads?: [#Ident]: {
		replicas?:  #PosInt | null
		resources?: {memory?: #Size | null, cpus?: #Cpus | null, #X} | null
		env?: {[#EnvName]: #Scalar | null} | null
		strategy?: "rolling" | "recreate" | null
		routes?: [...#Route] | null
		#X
	}
	services?: [#Ident]: {
		resources?: {memory?: #Size | null, cpus?: #Cpus | null, #X} | null
		settings?: {[string]: #Scalar | null} | null
		backup?: {
			schedule?:       #Schedule | null
			retention_days?: #Days | null
			restore_drill?:  {schedule: #Schedule, #X} | null
			destination?:    string | null
			#X
		} | null
		#X
	}
	#X
}

#Environment: {
	server:     #Server
	base_path?: #AbsPath
	policy:     #Policy
	overrides?: #Overrides
	#X
}

// ---------- verification ----------

#VerifyHTTP: {workload: #Ident, http: #UrlPath, port?: #Port, exec?: _|_, url?: _|_, migration_revisions?: _|_, contains?: _|_, status_codes?: _|_, required_headers?: _|_, json_assertions?: _|_, #VerifyCommon}
#VerifyExec: {workload: #Ident, exec: string & !="", http?: _|_, url?: _|_, migration_revisions?: _|_, contains?: _|_, status_codes?: _|_, required_headers?: _|_, json_assertions?: _|_, #VerifyCommon}

#VerifyURL: {
	url: =~"^https?://"
	workload?:            _|_
	http?:                _|_
	exec?:                _|_
	migration_revisions?: _|_
	status_codes?: [...#StatusCode]
	required_headers?: {[string]: string}
	contains?: string & !=""
	json_assertions?: [...{path: string & !="", equals: #Scalar | null, #X}]
	#VerifyCommon
}

#VerifyMigration: {
	workload?:        _|_
	url?:             _|_
	http?:            _|_
	exec?:            _|_
	contains?:        _|_
	status_codes?:    _|_
	required_headers?: _|_
	json_assertions?: _|_
	migration_revisions: {
		job:       #Ident
		provider?: string & !=""
		applied_revisions: [...string & !=""]
		#X
	}
	#VerifyCommon
}

#VerifyCommon: {
	...
	advisory: bool | *false
	#X
}

#Verification: #VerifyHTTP | #VerifyExec | #VerifyURL | #VerifyMigration

// ---------- top level ----------

// Exactly one of: top-level shorthand describing a single workload, or an
// explicit non-empty workloads block. Never both, never neither.
#Config: {
	api_version: "onebox.run/v1"
	app:         #AppIdent
	base_path:   #AbsPath | *"/var/lib/ob"

	environments: {[#Ident]: #Environment}

	// Exactly one of: top-level shorthand describing a single workload, or a
	// non-empty explicit workloads block. Never both, never neither. Enforced by
	// the loader for the same reason routing exclusivity is.
	build?:   #Build
	image?:   #Image
	compose?: #ComposeRef
	domain?:  string & !=""
	port?:    #Port
	health?:  #Health
	routes?: [...#Route]
	workloads?: {[#Ident]: #Workload}

	services?: {[#Ident]: #Service}

	deployment: {
		order?: [...#Ident]
		retain_releases:  #PosInt | *5
		migration_policy: "manual" | "auto" | "expand-only" | *"manual"
		#X
	}

	// Repository files staged onto the target alongside the release, for
	// configuration a referenced Compose service mounts: ClickHouse XML, an
	// init script, a proxy's dynamic config. Without this the reference is
	// staged but the file it mounts is not.
	files?: [...#RepoPath]

	runtime?: {
		// Applied to every application and worker workload that does not declare
		// its own. Daemons never receive them.
		env_files?: [...#RepoPath]
		preflight?: [...{
			file: #RepoPath
			require?: [...#EnvName]
			present?: [...#EnvName]
			#X
		}]
		#X
	}

	hooks?: {
		bootstrap?:    #Command
		pre_release?:  #Command
		post_release?: #Command
		post_deploy?:  #Command
		#X
	}

	verification?: [...#Verification]

	notifications?: {[#Ident]: {
		webhook: =~"^https?://"
		on?: [..."success" | "failure"]
		format: "text" | "json" | *"text"
		#X
	}}

	registries?: {[#Ident]: {
		server:        string & !=""
		username?:     string & !=""
		password_env?: #EnvName
		#X
	}}

	proxy: {
		managed: bool | *true
		kind:    "traefik-docker" | "none" | *"traefik-docker"
		image?:  #ImageRef
		config?: #RepoPath
		network: string & !="" | *"ob-ingress"
		// Terminating TLS without naming a resolver yields a router that never
		// obtains a certificate. It belongs on the proxy: one ACME account
		// serves every route.
		cert_resolver?: string & !=""
		#X
	}

	secrets?: {[#Ident]: #RepoPath | {
		provider: "sops" | "age" | *"sops"
		file:     #RepoPath
		#X
	}}

	observability?: {
		logs?:    {enabled: bool | *false, retention_days?: #Days, #X}
		metrics?: {enabled: bool | *false, #X}
		alerts?:  {unhealthy_after?: #Dur, #X}
		#X
	}

	#X
}
