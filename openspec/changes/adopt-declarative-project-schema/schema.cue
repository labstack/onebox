// Normative shape for the onebox.run/v1 authoring contract.
//
// This file is part of the specification, not the implementation. It exists
// because prose cannot state requiredness, exclusivity, bounds, and closure
// precisely enough for two implementations to accept the same corpus — the
// finding that ended two review rounds. The behavioural requirements live in
// specs/project-schema/spec.md; the shape lives here, and it compiles.
//
// Path kinds are distinguished deliberately. #RepoPath is resolved against the
// directory holding the project file and may not escape the repository.
// #AbsPath is a path on the target. #UrlPath is a request path. Conflating
// them made the required default base path invalid under the old blanket rule.
package obschema

// ---------- scalars ----------

// One character is legal: an identifier of "a" is fine, and forbidding it
// would have to be relaxed later, which narrowing rules forbid.
#Ident: =~"^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$"

// The application identifier additionally reserves the generated namespace.
// Underscore is already excluded by #Ident and joins derived names.
#AppIdent: #Ident & !~"^ob-" & !="ob" & !="proxy" & !="_host"

#Port:    int & >0 & <65536
#PosInt:  int & >0
#Days:    int & >0
#Dur:     =~"^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$"
#Cron:    =~"^[-0-9*/,A-Za-z ]+$"
#TZ:      string & !=""
#Signal:  =~"^[A-Z][A-Z0-9]*$"
#Size:    =~"^[0-9]+(\\.[0-9]+)?(B|KB|MB|GB|TB)$"
#Cpus:    =~"^[0-9]+(\\.[0-9]+)?$"

// A repository-relative path. Never absolute, never escaping upward.
#RepoPath: string & !="" & !~"^/" & !~"(^|/)\\.\\.(/|$)"
// A path on the target host.
#AbsPath: =~"^/"
// A request path.
#UrlPath: =~"^/"

#ImageRef: string & !=""
#EnvName:  =~"^[A-Za-z_][A-Za-z0-9_]*$"
#Scalar:   string | int | float | bool

// Extension keys are accepted anywhere a mapping is accepted, and ignored.
#X: {[=~"^x-"]: _}

// ---------- sources ----------

#Build: #RepoPath | {
	context!:    #RepoPath
	dockerfile?: #RepoPath
	target?:     string & !=""
	args?: [#EnvName]: #Scalar
	platform?: string & !=""
	#X
}

#Image: #ImageRef | {
	reference!: #ImageRef
	platform?:  string & !=""
	pull?:      "always" | "missing" | "never" | *"missing"
	registry?:  #Ident // names an entry in registries
	#X
}

// file#service — the bounded escape hatch.
#ComposeRef: =~"^[^#]+#[a-zA-Z0-9._-]+$"

// ---------- health ----------

#Health: #UrlPath | {
	{http!: #UrlPath} | {exec!: string & !=""} | {tcp!: true}
	port?:         #Port
	interval?:     #Dur
	start_period?: #Dur
	within?:       #Dur
	retries?:      #PosInt
	#X
}

// ---------- routing ----------

// entrypoint names the listener the proxy exposes; port is the container port
// behind it. Separating them is what makes a non-HTTP listener expressible.
#Route: {
	domain!:     string & !=""
	path?:       #UrlPath | *"/"
	port!:       #Port
	entrypoint?: string & !=""
	protocol?:   "http" | "tcp" | *"http"
	tls?:        "terminate" | "passthrough" | "none" | *"terminate"
	#X
}

// ---------- storage ----------

#Volume: #Ident | {
	name!: #Ident
	path?: #AbsPath
	mode?: "rw" | "ro" | *"rw"
	#X
}

#Persistence: {
	mode?: "durable" | "ephemeral" | "external" | *"durable"
	#X
}

// ---------- protection ----------

#Schedule: {
	cron!:     #Cron
	timezone?: #TZ | *"UTC"
	#X
}

#Protection: {
	backup?: {
		schedule!:       #Schedule
		retention_days?: #Days
		destination?:    string & !=""
		#X
	}
	restore_drill?: {
		schedule!: #Schedule
		#X
	}
	#X
}

// ---------- workloads ----------

// A command is a list, or the hook form when it must run somewhere specific.
#Command: [...string] | {
	run!:   string & !=""
	local?: bool | *false
	#X
}

#Workload: {
	role?: "application" | "worker" | "job" | *"application"

	// Exactly one source.
	{build!: #Build} | {image!: #Image} | {compose!: #ComposeRef}

	command?:  #Command
	replicas?: #PosInt | *1
	strategy?: "rolling" | "recreate"

	// Scalar shorthand for a single route; mutually exclusive with routes.
	domain?: string & !=""
	port?:   #Port
	routes?: [...#Route]

	health?:      #Health
	drain?:       {signal?: #Signal | *"TERM", wait?: #Dur, grace?: #Dur, #X}
	resources?:   {memory?: #Size, cpus?: #Cpus, #X}
	env?:         {[#EnvName]: #Scalar}
	volumes?:     [...#Volume]
	persistence?: #Persistence
	protection?:  #Protection

	// Declared prerequisites; a name in services or another workload.
	needs?: [...#Ident]

	// Job only. data_effect is required for a job and carries `unknown`,
	// which the previous contract accepted and an operator may honestly mean.
	run?:         "pre_release" | "post_release" | "manual"
	data_effect?: "none" | "migration" | "destructive" | "unknown"
	#X
}

// ---------- services ----------

#Backup: {
	schedule?:       #Schedule
	retention_days?: #Days
	restore_drill?:  {schedule!: #Schedule, #X}
	destination?:    string & !=""
	#X
}

// Volume identifiers are declared, not invented, because their generated
// names are permanent and must be reservable before any driver exists.
#Service: string | int | {
	driver?:  #Ident
	version!: string | int
	volumes?: [...#Ident]
	persistence?: #Persistence
	resources?:   {memory?: #Size, cpus?: #Cpus, #X}
	settings?:    {[string]: #Scalar}
	backup?:      #Backup
	#X
}

// ---------- environments ----------

#Server: string & !="" | {
	host!: string & !=""
	user?: string & !=""
	port?: #Port
	#X
}

#Policy: {
	require_approval?:               bool | *true
	allow_agent_proposals?:          bool | *true
	minimum_onebox_version?:         =~"^v[0-9]{4}\\.(0[1-9]|1[0-2])\\.[1-9][0-9]*$"
	minimum_plan_schema?:            string & !=""
	require_migration_backup?:       bool | *false
	migration_backup_max_age?:       #Dur
	require_migration_restore_test?: bool | *false
	migration_backup_key_material?: [...string]
	#X
}

#Overrides: {
	workloads?: [#Ident]: {
		replicas?:  #PosInt | null
		resources?: {memory?: #Size | null, cpus?: #Cpus | null, #X} | null
		env?:       {[#EnvName]: #Scalar | null} | null
		strategy?:  "rolling" | "recreate" | null
		routes?: [...#Route] | null
		#X
	}
	services?: [#Ident]: {
		resources?: {memory?: #Size | null, cpus?: #Cpus | null, #X} | null
		settings?:  {[string]: #Scalar | null} | null
		backup?:    #Backup | null
		#X
	}
	#X
}

#Environment: {
	server!:    #Server
	base_path?: #AbsPath
	policy?:    #Policy
	overrides?: #Overrides
	#X
}

// ---------- verification ----------

#Verification: {
	{workload!: #Ident} | {url!: =~"^https?://"} | {migration_revisions!: {
		job!:      #Ident
		provider?: string & !=""
		applied_revisions!: [...string]
		#X
	}}
	http?:   #UrlPath
	exec?:   string & !=""
	port?:   #Port
	status_codes?: [...#Port]
	required_headers?: {[string]: string}
	contains?: string & !=""
	json_assertions?: [...{path!: string & !="", equals!: #Scalar, #X}]
	advisory?: bool | *false
	#X
}

// ---------- top level ----------

#Config: {
	api_version!: "onebox.run/v1"
	app!:         #AppIdent
	base_path?:   #AbsPath | *"/var/lib/ob"

	environments!: {[#Ident]: #Environment}

	// Top-level workload shorthand. Mutually exclusive with `workloads`.
	build?:  #Build
	image?:  #Image
	compose?: #ComposeRef
	port?:   #Port
	health?: #Health
	domain?: string & !=""

	workloads?: {[#Ident]: #Workload}
	services?:  {[#Ident]: #Service}

	deployment?: {
		order?: [...#Ident]
		retain_releases?:  #PosInt | *5
		migration_policy?: "manual" | "auto" | "expand-only" | *"manual"
		#X
	}

	runtime?: {
		env_files?: [...#RepoPath]
		preflight?: [...{
			file!: #RepoPath
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
		webhook!: =~"^https?://"
		on?: [..."success" | "failure"]
		format?: "text" | "json" | *"text"
		#X
	}}

	registries?: {[#Ident]: {
		server!:       string & !=""
		username?:     string & !=""
		password_env?: #EnvName
		#X
	}}

	proxy?: {
		managed?: bool | *true
		kind?:    "traefik-docker" | "none" | *"traefik-docker"
		image?:   #ImageRef
		config?:  #RepoPath
		network?: string & !="" | *"ob-ingress"
		#X
	}

	secrets?: {[#Ident]: {
		provider!: "sops" | "age"
		file!:     #RepoPath
		#X
	}}

	observability?: {
		logs?:    {enabled?: bool | *false, retention_days?: #Days, #X}
		metrics?: {enabled?: bool | *false, #X}
		alerts?:  {unhealthy_after?: #Dur, #X}
		#X
	}

	#X
}
