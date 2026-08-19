package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/shellquote"
)

// Supporting services are the reason this tool exists. Nearly every
// application needs a database and a cache, nobody's application *is* the
// database, and writing the same Postgres service block into every project is
// work that teaches you nothing.
//
// So a project says `services: {postgres: 17}` and Onebox owns the rest: the
// image, the data volume, the health check, the credential, and the connection
// string the application reads. What it owns, it must own completely — a
// half-managed database that still needs the operator to invent a password is
// worse than none, because it looks handled.
//
// Three rules hold this together.
//
// A service is not part of the application's release. It gets its own Compose
// project, so no deploy and no rollback can take it down or remove its volume.
// The application can be replaced a hundred times underneath a database that
// never restarts.
//
// A credential is generated on the target, once, and never travels. It is not
// in the project, not in the rendered runtime, and not in the digest — so the
// rendered runtime stays a pure function of the declaration, and reading a
// release tells you nothing you should not know.
//
// An unrecognised driver is refused, not improvised. Guessing an image name
// from an identifier would produce a container that starts and stores nothing
// durable, discovered at the worst possible time.

// driver describes how one kind of service is run. Everything Onebox knows
// about Postgres lives in one struct, so adding a service is a data change.
type driver struct {
	// image is the repository; version comes from the declaration and is never
	// defaulted — an unpinned database version is a future surprise.
	image string
	// port is the service's own port, used for the connection string.
	port int
	// dataPath is where the durable data lives inside the container. Empty
	// means the driver stores nothing worth keeping.
	dataPath string
	// health is the container health check. A driver with none cannot be
	// waited for, and `needs` degrades to "started" rather than pretending.
	health []string
	// command is a fixed argument list the server needs to run at all.
	command []string
	// env is the fixed environment, plus a `%s` credential placeholder in
	// passwordEnv. secretEnv names the variables written to the target-side
	// credential file rather than into the rendered runtime.
	env       map[string]string
	secretEnv []string
	// scheme builds the client URL: scheme://user:password@host:port/database.
	scheme string
	// user is the identity the service is created with, and database is the
	// application name, so two projects on one host cannot end up sharing a
	// database by accident.
	user string
	// urlQuery is appended to the connection string. Some drivers need a
	// parameter to be usable at all: a Mongo root user created through
	// MONGO_INITDB_ROOT_USERNAME lives in the `admin` database, so a URL that
	// selects the application database authenticates against the wrong one.
	urlQuery string
	// majorUpgradeInPlace is whether the driver can read a data directory
	// written by a previous major version. A database that cannot needs a dump
	// and restore, which Onebox does not perform — so it refuses the change
	// rather than replacing the container and leaving it crash-looping.
	majorUpgradeInPlace bool
	// database is whether the driver has a named database at all. Redis and a
	// message broker do not, and reporting one would hand an application a
	// value that means nothing.
	database bool
	// urlUser is the user the connection URL embeds, which is not always the
	// user the service was created with: Redis authenticates the built-in
	// `default` user, and a URL with an empty username fails AUTH outright.
	// Empty means the URL carries no credential at all — the key travels as
	// its own variable, because a header is not a URL.
	urlUser string
	// settings maps declared settings onto the driver's own mechanism.
	settings settingsForm
	// persistenceOptions are the server options this driver derives from
	// persistence.mode, keyed by mode. They are defaults: an authored setting of
	// the same name replaces the value rather than being appended beside it, so
	// the rendered command carries one value per option.
	//
	// A driver with none leaves its server's own defaults alone; the mode still
	// decides whether Onebox gives it a durable volume.
	persistenceOptions map[string]map[string]string
}

type settingsForm int

const (
	// settingsEnv passes each setting as an uppercased environment variable.
	settingsEnv settingsForm = iota
	// settingsPostgres passes `-c key=value` server arguments.
	settingsPostgres
	// settingsRedisFlag passes `--key value` server arguments.
	settingsRedisFlag
	// settingsUnsupported refuses settings for drivers with no safe mechanism,
	// rather than accepting values it would then ignore.
	settingsUnsupported
)

// serviceIsEphemeral reports a service the author declared disposable.
//
// persistence.mode is the declaration Onebox can act on, and until now no
// managed service consulted it: every driver rendered a durable volume whatever
// the mode said, so `mode: ephemeral` changed nothing at all. The mode owns
// this decision and a driver setting cannot override it — a server flag has no
// business redefining what Onebox claims about data lifetime.
//
// Omitted persistence stays durable, which is what defaults.go already fills in,
// so existing projects render exactly as before.
func serviceIsEphemeral(s Service) bool {
	return s.Persistence != nil && s.Persistence.Mode == "ephemeral"
}

// drivers is the catalogue. A name that is not here is refused by the loader.
var drivers = map[string]driver{
	"postgres": {
		database: true,
		image:    "postgres", port: 5432, dataPath: "/var/lib/postgresql/data",
		health:    []string{"CMD-SHELL", "pg_isready -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\""},
		env:       map[string]string{"PGDATA": "/var/lib/postgresql/data/pgdata"},
		secretEnv: []string{"POSTGRES_PASSWORD"},
		urlUser:   "onebox", scheme: "postgres", user: "onebox", settings: settingsPostgres,
	},
	"mysql": {
		database: true,
		image:    "mysql", port: 3306, dataPath: "/var/lib/mysql",
		health:    []string{"CMD-SHELL", "mysqladmin ping -h 127.0.0.1 --silent"},
		secretEnv: []string{"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD"},
		urlUser:   "onebox", scheme: "mysql", user: "onebox", settings: settingsUnsupported,
	},
	"mariadb": {
		database: true,
		image:    "mariadb", port: 3306, dataPath: "/var/lib/mysql",
		health:    []string{"CMD-SHELL", "healthcheck.sh --connect --innodb_initialized"},
		secretEnv: []string{"MARIADB_PASSWORD", "MARIADB_ROOT_PASSWORD"},
		urlUser:   "onebox", scheme: "mysql", user: "onebox", settings: settingsUnsupported,
	},
	"redis": {
		majorUpgradeInPlace: true,
		image:               "redis", port: 6379, dataPath: "/data",
		// A write, not a PING. Redis answers PONG while refusing every write when
		// a background save has failed and stop-writes-on-bgsave-error is on —
		// which is its own default — so a connection-only probe reports healthy
		// exactly when the thing callers need is gone, and a health-gated
		// rollout converges onto it. SET proves the write path; EX bounds the
		// key so the probe cannot accumulate one.
		health:    []string{"CMD-SHELL", "redis-cli -a \"$REDIS_PASSWORD\" set ob:health 1 EX 30 | grep -qx OK"},
		command:   []string{"sh", "-c", "exec redis-server --requirepass \"$REDIS_PASSWORD\""},
		secretEnv: []string{"REDIS_PASSWORD"},
		urlUser:   "default", scheme: "redis", settings: settingsRedisFlag,
		// Durable keeps the append-only log this driver has always run with.
		// Ephemeral turns off both persistence mechanisms: a service declared
		// disposable should not pay an fsync per write, nor grow an AOF in a
		// container layer nothing intends to read back.
		persistenceOptions: map[string]map[string]string{
			"durable":   {"appendonly": "yes"},
			"ephemeral": {"appendonly": "no", "save": ""},
		},
	},
	"valkey": {
		majorUpgradeInPlace: true,
		image:               "valkey/valkey", port: 6379, dataPath: "/data",
		// Same failure mode and the same probe as redis; see the note there.
		health:    []string{"CMD-SHELL", "valkey-cli -a \"$REDIS_PASSWORD\" set ob:health 1 EX 30 | grep -qx OK"},
		command:   []string{"sh", "-c", "exec valkey-server --requirepass \"$REDIS_PASSWORD\""},
		secretEnv: []string{"REDIS_PASSWORD"},
		urlUser:   "default", scheme: "redis", settings: settingsRedisFlag,
		// Same contract as redis; see the note there.
		persistenceOptions: map[string]map[string]string{
			"durable":   {"appendonly": "yes"},
			"ephemeral": {"appendonly": "no", "save": ""},
		},
	},
	"mongodb": {
		database: true,
		image:    "mongo", port: 27017, dataPath: "/data/db",
		health:    []string{"CMD-SHELL", "mongosh --quiet --eval 'db.adminCommand(\"ping\").ok' | grep -q 1"},
		secretEnv: []string{"MONGO_INITDB_ROOT_PASSWORD"},
		urlUser:   "onebox", scheme: "mongodb", user: "onebox", settings: settingsUnsupported,
		// The root user is created in `admin`; without this the client tries
		// to authenticate against the application database and is refused.
		urlQuery: "authSource=admin",
		// This is a standalone server. Change streams and multi-document
		// transactions require a replica set, so an application using either
		// connects, authenticates, and then fails in its own logs — Rocket.Chat
		// with "The $changeStream stage is only supported on replica sets".
		// Configuring one needs a keyfile for internal auth, a one-time
		// rs.initiate, a health check that waits for PRIMARY rather than for a
		// ping, and replicaSet= on every connection string. That is a
		// service-initialisation step this driver model does not have, so the
		// limitation is documented in the schema guide rather than hidden.
	},
	"rabbitmq": {
		majorUpgradeInPlace: true,
		image:               "rabbitmq", port: 5672, dataPath: "/var/lib/rabbitmq",
		health:    []string{"CMD-SHELL", "rabbitmq-diagnostics -q ping"},
		secretEnv: []string{"RABBITMQ_DEFAULT_PASS"},
		urlUser:   "onebox", scheme: "amqp", user: "onebox", settings: settingsEnv,
	},
	"minio": {
		majorUpgradeInPlace: true,
		image:               "minio/minio", port: 9000, dataPath: "/data",
		health:    []string{"CMD-SHELL", "mc ready local"},
		command:   []string{"server", "/data", "--console-address", ":9001"},
		secretEnv: []string{"MINIO_ROOT_PASSWORD"},
		urlUser:   "onebox", scheme: "s3", user: "onebox", settings: settingsEnv,
	},
	"meilisearch": {
		majorUpgradeInPlace: true,
		image:               "getmeili/meilisearch", port: 7700, dataPath: "/meili_data",
		health:    []string{"CMD-SHELL", "curl -fsS http://localhost:7700/health"},
		env:       map[string]string{"MEILI_ENV": "production"},
		secretEnv: []string{"MEILI_MASTER_KEY"},
		scheme:    "http", settings: settingsEnv,
	},
	"clickhouse": {
		database: true,
		image:    "clickhouse/clickhouse-server", port: 8123, dataPath: "/var/lib/clickhouse",
		health:    []string{"CMD-SHELL", "clickhouse-client --query 'SELECT 1'"},
		secretEnv: []string{"CLICKHOUSE_PASSWORD"},
		urlUser:   "onebox", scheme: "http", user: "onebox", settings: settingsEnv,
	},
	"nats": {
		majorUpgradeInPlace: true,
		image:               "nats", port: 4222, dataPath: "/data",
		// The nats image carries no shell utilities, so there is nothing to run
		// as a health check. `needs` on it resolves to "started" rather than
		// waiting forever on a condition the container can never report.
		command: []string{"--jetstream", "--store_dir", "/data"},
		scheme:  "nats", settings: settingsUnsupported,
	},
}

// UpgradeInPlace reports whether a service's driver can start on a data
// directory written by a previous major version.
func (p *Spec) UpgradeInPlace(name string) bool {
	s, ok := p.Services[name]
	if !ok {
		return true
	}
	_, d, known := driverOf(name, s)
	return !known || d.dataPath == "" || d.majorUpgradeInPlace
}

// DeclaredVersion is the version a service declares, as written.
func (p *Spec) DeclaredVersion(name string) string {
	s, ok := p.Services[name]
	if !ok {
		return ""
	}
	return versionString(s.Version)
}

// MajorOf is the leading component of a version, which is the part that decides
// whether a data directory can still be read.
func MajorOf(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return major
}

// DriverNames lists the catalogue, for an error message that tells the author
// what they can actually write.
func DriverNames() []string {
	out := make([]string, 0, len(drivers))
	for name := range drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// driverOf resolves a service's driver. The name is the driver unless the
// declaration says otherwise, so `services: {postgres: 17}` needs no more
// than that, and `services: {events: {driver: postgres, version: 17}}` names
// a second one.
func driverOf(name string, s Service) (string, driver, bool) {
	key := s.Driver
	if key == "" {
		key = name
	}
	d, ok := drivers[key]
	return key, d, ok
}

// HasHealth reports whether the service's driver can be waited for. A `needs`
// on a service with no health check resolves to "started": claiming otherwise
// would produce a runtime that never starts.
func (p *Spec) serviceHasHealth(name string) bool {
	s, ok := p.Services[name]
	if !ok {
		return false
	}
	_, d, known := driverOf(name, s)
	return known && len(d.health) > 0
}

// renderService generates one service's Compose document. It is its own
// project, so it survives every release of the application beside it.
func (p *Spec) renderService(n Names, name string, s Service, selectedImage string, protection *serviceProtection) ([]byte, error) {
	key, d, ok := driverOf(name, s)
	if !ok {
		return nil, errf("unknown_service_driver", "services."+name, "", strings.Join([]string{
			"no managed driver named %q; Onebox runs these: %s.",
			"To run something else, declare it as a daemon workload — you own the image and the settings then.",
		}, "\n"), key, strings.Join(DriverNames(), ", "))
	}

	image := selectedImage
	if image == "" {
		image = d.image + ":" + versionString(s.Version)
	}
	if err := checkImageRef("services."+name+".version", image); err != nil {
		return nil, err
	}
	svc := map[string]any{
		"image":          image,
		"restart":        "unless-stopped",
		"container_name": n.ServiceContainer(name),
		"labels": map[string]any{
			"ob.app":     p.Name,
			"ob.service": name,
			"ob.driver":  key,
		},
		"networks": []string{n.ServiceNetwork()},
		// The credential file is written on the target and never travels with
		// the release, so the password is absent from this document and from
		// its digest.
		"env_file": []string{n.ServiceSecretFile(name)},
	}

	// The protection credential file comes second so its entries are present
	// alongside the service credential, not instead of it: Compose applies each
	// env_file over the ones before it, and the two name disjoint variables.
	if protection != nil {
		svc["env_file"] = []string{n.ServiceSecretFile(name), protection.CredentialFile}
	}

	env := map[string]any{}
	for k, v := range d.env {
		env[k] = v
	}
	if d.user != "" {
		for k, v := range identityEnv(key, d, p.Name) {
			env[k] = v
		}
	}
	command := append([]string(nil), d.command...)

	if err := applySettings(name, d, s, s.Settings, env, &command); err != nil {
		return nil, err
	}

	// Protection is applied over authored settings rather than under them. The
	// archive configuration is not a default a project may prefer differently:
	// a server whose archive_mode an author turned back off would keep running
	// while its recovery window silently stopped advancing.
	if protection != nil {
		// The entry *names* only. Values live in the mode-0600 credential file
		// on the host and are read by the image's entrypoint, so no secret
		// enters this document or its digest.
		env["OB_S3_KEY_ENTRY"] = protection.KeyEntry
		env["OB_S3_SECRET_ENTRY"] = protection.SecretEntry
		if protection.SessionEntry != "" {
			env["OB_S3_SESSION_TOKEN_ENTRY"] = protection.SessionEntry
		}
		if len(command) == 0 {
			// The official entrypoint dispatches on argv[0], so the server has
			// to be named again once this document supplies a command at all.
			command = []string{"postgres"}
		}
		command = append(command,
			"-c", "archive_mode=on",
			"-c", "archive_command="+protection.ArchiveCommand,
			"-c", "archive_timeout="+protection.ArchiveTimeout,
			"-c", "wal_level=replica",
		)
	}
	if len(env) > 0 {
		svc["environment"] = env
	}
	if len(command) > 0 {
		svc["command"] = command
	}
	if len(d.health) > 0 {
		svc["healthcheck"] = map[string]any{
			"test": d.health, "interval": "5s", "timeout": "5s",
			"retries": 12, "start_period": "10s",
		}
	}

	volumes := map[string]any{}
	if d.dataPath != "" && !serviceIsEphemeral(s) {
		vol := dataVolume(s)
		full := n.ServiceVolume(name, vol)
		mounts := []string{full + ":" + d.dataPath}
		if protection != nil {
			// The directory, not the file — see ServiceProtectionConfigDir for
			// why an atomically replaced file vanishes from a running
			// container. Read-only, because a container that could rewrite its
			// own configuration could point its repository somewhere the
			// project never declared.
			mounts = append(mounts, protection.ConfigHostDir+":"+PgBackRestConfDir+":ro")
		}
		svc["volumes"] = mounts
		volumes[full] = map[string]any{
			"name":   full,
			"labels": map[string]any{"ob.app": p.Name, "ob.service": name},
		}
	}
	if s.Resources != nil {
		if s.Resources.Memory != "" {
			svc["mem_limit"] = s.Resources.Memory
		}
		if s.Resources.CPUs != "" {
			svc["cpus"] = s.Resources.CPUs
		}
	}

	// The whole document is generated, and the shell expansions in it are meant
	// for the container, not for Compose on the host.
	doc := map[string]any{
		"name":     n.ServiceProject(name),
		"services": map[string]any{name: escapeDollars(svc)},
		"networks": map[string]any{n.ServiceNetwork(): map[string]any{"external": true}},
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	body, err := marshalDeterministic(doc)
	if err != nil {
		return nil, err
	}
	header := "# Generated by Onebox for service " + name + ". It is its own Compose\n" +
		"# project: no release and no rollback of the application can stop it or\n" +
		"# remove its data.\n"
	return append([]byte(header), body...), nil
}

// identityEnv is the user and database the service is created with, under the
// variable names each driver expects. Both are the application name, so two
// projects on one host cannot silently share a database.
func identityEnv(key string, d driver, app string) map[string]any {
	switch key {
	case "postgres":
		return map[string]any{"POSTGRES_USER": d.user, "POSTGRES_DB": app}
	case "mysql":
		return map[string]any{"MYSQL_USER": d.user, "MYSQL_DATABASE": app}
	case "mariadb":
		return map[string]any{"MARIADB_USER": d.user, "MARIADB_DATABASE": app}
	case "mongodb":
		return map[string]any{"MONGO_INITDB_ROOT_USERNAME": d.user}
	case "rabbitmq":
		return map[string]any{"RABBITMQ_DEFAULT_USER": d.user}
	case "minio":
		return map[string]any{"MINIO_ROOT_USER": d.user}
	case "clickhouse":
		return map[string]any{"CLICKHOUSE_USER": d.user, "CLICKHOUSE_DB": app}
	}
	return nil
}

// applySettings puts declared settings where the driver actually reads them.
// A driver with no safe mechanism refuses rather than accepting values it
// would silently ignore.
func applySettings(name string, d driver, s Service, settings map[string]any, env map[string]any, command *[]string) error {
	// The mode's options first, then the authored ones over them. Merging here
	// rather than appending is what keeps one value per option in the rendered
	// command: appending produced `--appendonly yes --appendonly no` for an
	// author who set the flag the driver had already fixed.
	effective := map[string]any{}
	// Only ephemeral turns persistence off. Every other mode — durable,
	// external, and any mode added later — keeps the durable options, because a
	// mode this table does not know about must not silently disable a server's
	// persistence. `external` is exactly that case: it means the operator covers
	// this data, and reading it as "no options" rendered a durable volume with
	// the append-only log switched off.
	options := d.persistenceOptions["durable"]
	if serviceIsEphemeral(s) {
		options = d.persistenceOptions["ephemeral"]
	}
	for k, v := range options {
		effective[k] = v
	}
	for k, v := range settings {
		effective[k] = v
	}
	if len(effective) == 0 {
		return nil
	}
	// A driver with no mechanism refuses only what the author asked for. Mode
	// options are Onebox's own and are never declared for such a driver, so
	// this cannot refuse a project that set nothing.
	if d.settings == settingsUnsupported && len(settings) == 0 {
		return nil
	}
	settings = effective
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	switch d.settings {
	case settingsEnv:
		for _, k := range keys {
			env[strings.ToUpper(k)] = settings[k]
		}
	case settingsPostgres:
		if len(*command) == 0 {
			*command = []string{"postgres"}
		}
		for _, k := range keys {
			*command = append(*command, "-c", fmt.Sprintf("%s=%v", k, settings[k]))
		}
	case settingsRedisFlag:
		// The server command is a single shell string for this driver, so a
		// setting is appended to it rather than to an argument list.
		last := len(*command) - 1
		for _, k := range keys {
			(*command)[last] += fmt.Sprintf(" --%s %q", k, fmt.Sprint(settings[k]))
		}
	case settingsUnsupported:
		return errf("service_settings_unsupported", "services."+name+".settings", "",
			"the %s driver has no setting mechanism Onebox can apply safely; "+
				"declare it as a daemon workload if you need to configure it", d.image)
	}
	return nil
}

// ClientEnv is the connection information an application reads. It names the
// variables Onebox writes on the target; the values contain the generated
// credential and so are produced there, never here.
type ClientEnv struct {
	Service string
	Driver  string
	// Prefix names the variable family: <PREFIX>_URL and its parts. An
	// application that can parse a URL reads one variable; one that wants a
	// host and a password separately — which most database images do — reads
	// the parts, instead of being told to split a string in a shell.
	Prefix   string
	Scheme   string
	User     string
	Database string
	Host     string
	Port     int
	// Query is appended to the connection string, without a leading "?".
	Query string
	// SecretVars are the credential variables, in the order the driver's
	// credential file declares them. The first is the one the URL embeds.
	SecretVars []string
}

// ClientEnvFor describes how to reach each declared service. The target
// materialises these into a file the workloads read, which is why nothing here
// carries a password.
func (p *Spec) ClientEnvFor(name string) (ClientEnv, bool) {
	s, ok := p.Services[name]
	if !ok {
		return ClientEnv{}, false
	}
	key, d, known := driverOf(name, s)
	if !known {
		return ClientEnv{}, false
	}
	return ClientEnv{
		Service: name, Driver: key, Prefix: envPrefix(name),
		Scheme: d.scheme, User: d.urlUser, Database: databaseOf(d, p.Name),
		Host: name, Port: d.port, SecretVars: d.secretEnv, Query: d.urlQuery,
	}, true
}

// URLVar is the connection-string variable.
func (c ClientEnv) URLVar() string { return c.Prefix + "_URL" }

// HasCredentialInURL reports whether the connection string carries the
// password. It does not when the driver authenticates some other way — a
// search engine taking an API key in a header, say — and inventing a
// credentialled URL for those would hand applications something that cannot
// work.
func (c ClientEnv) HasCredentialInURL() bool { return c.User != "" }

// databaseOf is the application's own database on drivers that have one.
func databaseOf(d driver, app string) string {
	if !d.database {
		return ""
	}
	return app
}

func envPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// dataVolume is the name of the service's durable volume. The first declared
// volume is that volume; a service that declares none gets `data`, which the
// loader writes into the project so the name appears in the canonical form
// rather than only in the generated runtime.
func dataVolume(s Service) string {
	if len(s.Volumes) > 0 {
		return s.Volumes[0]
	}
	return "data"
}

func versionString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// A YAML-decoded integer arrives as a float; `17` must not become
		// `postgres:17.000000`.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
	}
	return fmt.Sprint(v)
}

// ServicePublicEnv names the environment variables in a service's document
// that Onebox chose itself: the data directory, the user, the database. None
// can be a secret, because every secret lives in the credential file.
//
// A preview redacts everything else — a declared setting is as likely to be a
// token as a tuning parameter. Redacting Onebox's own choices as well would
// make the preview useless and teach people to reach for --raw, which is the
// opposite of what redaction is for.
func (p *Spec) ServicePublicEnv(name string) []string {
	svc, ok := p.Services[name]
	if !ok {
		return nil
	}
	key, d, known := driverOf(name, svc)
	if !known {
		return nil
	}
	var out []string
	for k := range d.env {
		out = append(out, k)
	}
	for k := range identityEnv(key, d, p.Name) {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AliasFile is one workload's names for a service's connection.
type AliasFile struct {
	Path string
	// Vars maps the variable the workload reads to the connection part that
	// belongs in it.
	Vars map[string]string
}

// ClientEnvScript is the shell that establishes a service's credential on the
// target and writes every file derived from it.
//
// The password is generated here because here is the only place it may exist:
// not in the project, not in the rendered runtime, not in the digest.
//
// It is generated once and then reused, never rotated — an application holding
// a credential its database has forgotten is a worse outage than any it would
// prevent. Everything derived from it is rewritten on every apply, so adding a
// workload, renaming a variable, or changing the connection shape takes effect
// without the password moving.
func (c ClientEnv) ClientEnvScript(secretFile, clientFile string, aliases []AliasFile) string {
	var b strings.Builder

	// Reuse the established password when there is one. Reading it back is
	// what lets the derived files be rewritten without rotating it.
	primary := c.Prefix + "_PASSWORD"
	if len(c.SecretVars) > 0 {
		primary = c.SecretVars[0]
	}
	fmt.Fprintf(&b, "if [ -s %s ]; then\n", shellQuote(secretFile))
	fmt.Fprintf(&b, "  pw=$(sed -n 's/^%s=//p' %s | head -n1)\n", primary, shellQuote(secretFile))
	b.WriteString("else\n")
	b.WriteString("  pw=$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \\n')\n")
	b.WriteString("fi\n")
	b.WriteString("[ -n \"$pw\" ] || { echo 'no source of randomness on this host' >&2; exit 1; }\n")
	b.WriteString("umask 077\n")

	// Truncate rather than remove. The file is the service's Compose `env_file`,
	// so it has to exist even for a driver that declares no credential variables
	// — `nats` is one — and removing it would leave the following chmod to fail
	// on a missing path and Compose to refuse a mount it was promised.
	// Written to a temp file and renamed. Truncating in place cannot be made
	// safe with `||` guards: the truncation lands first, so a failure between it
	// and the last append leaves an empty or half-written credential that the
	// *next* apply reads back through the `[ -s ]` branch above — minting a
	// fresh password the database will reject, or canonicalising a truncated
	// one. Both report success. A rename is atomic, so the file is either the
	// old credential or the complete new one.
	b.WriteString(atomicEnvFile(secretFile, func(target string) string {
		var lines strings.Builder
		for _, v := range c.SecretVars {
			fmt.Fprintf(&lines, "if ! printf '%%s=%%s\\n' %s \"$pw\" >> %[2]s; then echo 'cannot write '%[2]s >&2; exit 1; fi\n",
				shellQuote(v), target)
		}
		return lines.String()
	}))

	parts := c.parts()
	b.WriteString(writeEnvFile(clientFile, c.canonicalNames(), parts))
	for _, alias := range aliases {
		names := map[string]string{}
		for variable, part := range alias.Vars {
			names[variable] = part
		}
		b.WriteString(writeEnvFile(alias.Path, names, parts))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// parts is the connection by part name. Values are shell fragments, so the
// password is a variable reference rather than a value.
func (c ClientEnv) parts() map[string]string {
	url := c.Scheme + "://"
	if c.HasCredentialInURL() {
		url += c.User + ":$pw@"
	}
	url += fmt.Sprintf("%s:%d", c.Host, c.Port)
	// Every driver that has a database puts it in the URL. Keying this off the
	// scheme dropped it for ClickHouse, whose scheme is http, and handed
	// applications a connection string pointing at the server with no database
	// selected — which fails only once something queries.
	if c.Database != "" {
		url += "/" + c.Database
	}
	if c.Query != "" {
		url += "?" + c.Query
	}
	return map[string]string{
		"url": url, "host": c.Host, "port": fmt.Sprint(c.Port),
		"user": c.User, "password": "$pw", "database": c.Database,
	}
}

// canonicalNames is the connection under the names Onebox chose, which every
// workload that needs the service receives whether or not it asked for them.
func (c ClientEnv) canonicalNames() map[string]string {
	out := map[string]string{
		c.URLVar():             "url",
		c.Prefix + "_HOST":     "host",
		c.Prefix + "_PORT":     "port",
		c.Prefix + "_PASSWORD": "password",
	}
	if c.User != "" {
		out[c.Prefix+"_USER"] = "user"
	}
	if c.Database != "" {
		out[c.Prefix+"_DATABASE"] = "database"
	}
	return out
}

// writeEnvFile emits the shell that writes one env file, sorted so a re-apply
// produces the same bytes.
func writeEnvFile(path string, names map[string]string, parts map[string]string) string {
	// Same rule as the credential file: an application reading a half-written
	// connection file fails against a service that reports itself healthy.
	vars := make([]string, 0, len(names))
	for v := range names {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	return atomicEnvFile(path, func(target string) string {
		var lines strings.Builder
		for _, v := range vars {
			value, ok := parts[names[v]]
			if !ok || value == "" {
				// A part this driver does not have — a user for Redis, a database
				// for a cache. Writing it empty would look like a value.
				continue
			}
			fmt.Fprintf(&lines, "if ! printf '%%s=%%s\\n' %s \"%s\" >> %[3]s; then echo 'cannot write '%[3]s >&2; exit 1; fi\n",
				shellQuote(v), value, target)
		}
		return lines.String()
	})
}

// atomicEnvFile renders writes into a sibling temp file, then renames it over
// the target. Every step is checked with `if !`, not `||`: `:` and other POSIX
// special builtins abort a non-interactive shell on a redirection error before
// `||` can run, so a guard written that way is dead on dash — which is /bin/sh
// on Debian and Ubuntu. The temp file shares the target's directory so the
// rename cannot cross a filesystem.
func atomicEnvFile(path string, body func(target string) string) string {
	quoted, temp := shellQuote(path), shellQuote(path+".ob-tmp")
	var b strings.Builder
	fmt.Fprintf(&b, "if ! printf '' > %[1]s; then echo 'cannot write '%[1]s >&2; exit 1; fi\n", temp)
	// Every append is checked too. The rename is only reached when the temp file
	// is complete, so a short write can never be installed over a good file.
	b.WriteString(body(temp))
	fmt.Fprintf(&b, "if ! chmod 600 %[1]s; then echo 'cannot secure '%[1]s >&2; exit 1; fi\n", temp)
	fmt.Fprintf(&b, "if ! mv -f %[1]s %[2]s; then echo 'cannot install '%[2]s >&2; exit 1; fi\n", temp, quoted)
	return b.String()
}

// shellQuote wraps a value in single quotes for the remote shell. Every value
// it receives is a derived name or a variable name, never user content, but
// quoting them keeps the generated script readable and the rule uniform.
func shellQuote(s string) string {
	return shellquote.Quote(s)
}
