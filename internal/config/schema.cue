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
	service!:   #Ident
	mode!:      "rolling" | "recreate"
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

#Config: {
	app!:     =~"^[a-z][a-z0-9-]*$"
	compose!: string
	// exactly one host: yeet is single-host by design
	environments!: {[string]: {hosts!: [string]}}
	roles!: {[#Ident]: #Role}
	order?: [...#Ident]
	accessories?: [...#Ident]
	jobs?: [...#Ident]
	hooks?: {[#Ident]: #Hook}
	verify?: [...#Verify]
	proxy?: {kind?: string, managed?: bool}
	registry?:   #Registry
	retain?:     int & >0
	migrations?: "expand-only"
}
