// Onebox v1 authoring schema — YAML outside, CUE inside.
//
// V1 is additive-only: existing fields and their meanings are stable. New
// capabilities may be introduced as optional sections, while CUE continues to
// own shape, enums, and scalar patterns. Cross-field and compose-semantic
// checks stay in Go (config.Validate / compose.Classify).

#Ident: =~"^[a-zA-Z0-9][a-zA-Z0-9._-]*$"
#Dur:   =~"^(([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+|[0-9]+d)$"

#HTTPReadiness: {
	http!:         string & =~"^/"
	port!:         int & >0 & <65536
	interval?:     #Dur
	start_period?: #Dur
	within?:       #Dur
	retries?:      int & >0
}

#ExecReadiness: {
	exec!:         string & !=""
	interval?:     #Dur
	start_period?: #Dur
	within?:       #Dur
	retries?:      int & >0
}

#Readiness: #HTTPReadiness | #ExecReadiness

#Drain: {
	signal?: =~"^[A-Z0-9]+$"
	wait?:   #Dur
	grace?:  #Dur
}

#Hook: (string & !="") | {
	run!:   string & !=""
	local?: bool
}

#HTTPVerification: {
	component!: #Ident
	http!:      string & =~"^/"
	port?:     int & >0 & <65536
}

#ExecVerification: {
	component!: #Ident
	exec!:      string & !=""
}

#URLVerification: {
	url!:      string & =~"^https?://"
	contains?: string
	advisory?: bool
}

#Verification: #HTTPVerification | #ExecVerification | #URLVerification

#Registry: {
	server!:       string & =~"^[a-zA-Z0-9][a-zA-Z0-9.:-]*$"
	username!:     string & =~"^[a-zA-Z0-9][a-zA-Z0-9._-]*$"
	password_env!: string & =~"^[A-Z_][A-Z0-9_]*$"
}

#Preflight: {
	file!:     string
	require?: [...string]
	present?: [...string]
}

#EnvironmentPolicy: {
	require_approval?:      bool
	allow_agent_proposals?: bool
}

#Environment: {
	target!: string & !=""
	policy?: #EnvironmentPolicy
}

#ComponentDeployment: {
	strategy!: "rolling" | "recreate"
	replicas?: int & >0
}

#ApplicationComponent: {
	type!:       "application" | "worker"
	service?:    #Ident
	deployment!: #ComponentDeployment
	readiness?:  #Readiness
	drain?:      #Drain
	persistence?: #Persistence
	protection?:  #Protection
}

#JobComponent: {
	type!:        "job"
	service?:     #Ident
	command?:     #Hook
	data_effect!: "none" | "migration" | "unknown"
}

#Persistence: {
	mode!:     "durable" | "ephemeral" | "external"
	volumes?: [...#Ident]
}

#Schedule: {
	// POSIX five-field cron, evaluated in an explicit IANA timezone.
	cron!:     string & =~"^[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+$"
	timezone!: string & =~"^[A-Za-z0-9_+.-]+(/[A-Za-z0-9_+.-]+)*$"
}

#Backup: {
	schedule!:       #Schedule
	retention_days!: int & >0
}

#RestoreDrill: {
	schedule!: #Schedule
}

#Protection: {
	backup?:        #Backup
	restore_drill?: #RestoreDrill
}

#DataServiceComponent: {
	type!:        "postgres" | "mysql" | "redis"
	service?:     #Ident
	persistence!: #Persistence
	protection?:  #Protection
}

#GenericServiceComponent: {
	type!:        "service"
	service?:     #Ident
	persistence?: #Persistence
	protection?:  #Protection
}

#Component: #ApplicationComponent | #JobComponent | #DataServiceComponent | #GenericServiceComponent

#Deployment: {
	order?:            [...#Ident]
	retain_releases?:  int & >0
	migration_policy?: "manual" | "expand-only"
}

#Runtime: {
	env_files?: [...string]
	preflight?: [...#Preflight]
}

#Notifications: {
	webhook!: string & =~"^https?://"
	on?:      [...("failure" | "success")]
	format?:  "json" | "text"
}

#Proxy: {
	kind?:    "traefik-docker" | "none"
	managed?: bool
	image?:   string
	config?:  string
	network?: =~"^[a-z][a-z0-9-]*$"
}

#Secrets: {
	sops!: string
}

#Logs: {
	enabled!:        bool
	retention_days?: int & >0
}

#Metrics: {
	enabled!: bool
}

#Alerts: {
	unhealthy_after!: #Dur
}

#Hooks: {
	bootstrap?:    #Hook
	pre_release?:  #Hook
	post_release?: #Hook
	post_deploy?:  #Hook
}

#Observability: {
	logs?:    #Logs
	metrics?: #Metrics
	alerts?:  #Alerts
}

#Config: {
	api_version!: "onebox.run/v1"

	// app defaults to the project directory name and compose to the
	// conventional Compose filename.
	app?:     =~"^[a-z][a-z0-9-]*$"
	compose?: string

	environments!: {[#Ident]: #Environment}
	components!:   {[#Ident]: #Component}

	deployment?:   #Deployment
	runtime?:      #Runtime
	hooks?:        #Hooks
	verification?: [...#Verification]
	notifications?: #Notifications
	proxy?:         #Proxy
	secrets?:       #Secrets
	registry?:      #Registry
	observability?: #Observability
}
