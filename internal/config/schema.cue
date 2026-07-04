// yeet.yml schema — the CUE side of "YAML outside, CUE inside" (design §07).
// CUE owns shape, enums, and patterns; cross-field and compose-semantic
// checks stay in Go (config.Validate / compose.Classify).

#Ident: =~"^[a-zA-Z0-9._-]+$"
#Dur:   =~"^[0-9]+(ms|s|m|h)$"

#Ready: {
	http?:         string & =~"^/"
	exec?:         string
	port?:         int & >0 & <65536
	interval?:     #Dur
	start_period?: #Dur
	within?:       #Dur
}

#Drain: {
	signal?: =~"^[A-Z0-9]+$"
	wait?:   #Dur
}

#Role: {
	// both inferred when omitted: service defaults to the role key; mode is
	// derived from the compose service (rollable + healthcheck ⇒ rolling).
	service?:   #Ident
	mode?:      "rolling" | "recreate"
	replicas?:  int & >0
	singleton?: bool
	ready?:     #Ready
	drain?:     #Drain
}

#Hook: string | {
	run!:   string
	local?: bool
}

#Verify: {
	http?:     string & =~"^/"
	exec?:     string
	url?:      string & =~"^https?://"
	role?:     #Ident
	port?:     int & >0 & <65536
	contains?: string
	advisory?: bool
}

#Registry: {
	server!:       string & =~"^[a-zA-Z0-9.:-]+$"
	username!:     string & =~"^[a-zA-Z0-9._-]+$"
	password_env!: string & =~"^[A-Z_][A-Z0-9_]*$"
}

#Preflight: {
	file!:     string
	require?: [...string]
	present?: [...string]
}

#Config: {
	// app defaults to the project directory name; compose to the conventional
	// compose file; roles to every service not named an accessory or job.
	app?:     =~"^[a-z][a-z0-9-]*$"
	compose?: string
	// exactly one host: yeet is single-host by design
	environments!: {[string]: {hosts!: [string]}}
	roles?: {[#Ident]: #Role}
	order?: [...#Ident]
	accessories?: [...#Ident]
	jobs?: [...#Ident]
	env_files?: [...string]
	preflight?: [...#Preflight]
	hooks?: {[#Ident]: #Hook}
	verify?: [...#Verify]
	notify?: {
		webhook!: string & =~"^https?://"
		on?: [...("failure" | "success")]
		format?: "json" | "text"
	}
	proxy?: {
		kind?:    "traefik-docker" | "none"
		managed?: bool
		image?:   string
		config?:  string // dir with traefik.yml (+ dynamic.yml, .env); required when managed
		network?: =~"^[a-z][a-z0-9-]*$"
	}
	secrets?: {sops!: string}
	registry?:   #Registry
	retain?:     int & >0
	migrations?: "expand-only"
}
