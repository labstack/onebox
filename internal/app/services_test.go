package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func serviceSpec(t *testing.T, body string) *Spec {
	t.Helper()
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1, needs: [store]}
`+body), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func renderStore(t *testing.T, body string) string {
	t.Helper()
	r, err := serviceSpec(t, body).Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := r.RenderServices("production")
	if err != nil {
		t.Fatal(err)
	}
	return string(docs["store"])
}

// A service must be its own Compose project. Sharing the application's would
// mean a rollback could remove the database's volume.
func TestServiceIsItsOwnProject(t *testing.T) {
	doc := renderStore(t, "services: {store: {driver: postgres, version: 17}}\n")
	if !strings.Contains(doc, "name: ob_shop_store") {
		t.Fatalf("service is not in its own project:\n%s", doc)
	}
	if !strings.Contains(doc, "ob_shop_store_data:/var/lib/postgresql/data") {
		t.Fatalf("no durable volume at the driver's data path:\n%s", doc)
	}
	if !strings.Contains(doc, "external: true") {
		t.Fatalf("the shared network must be external — Compose would otherwise remove it:\n%s", doc)
	}
}

// The password is generated on the target. Nothing that travels may carry it.
func TestServiceDocumentCarriesNoCredential(t *testing.T) {
	doc := renderStore(t, "services: {store: {driver: postgres, version: 17}}\n")
	if strings.Contains(doc, "POSTGRES_PASSWORD:") {
		t.Fatalf("a credential reached the generated runtime:\n%s", doc)
	}
	if !strings.Contains(doc, "/var/lib/ob/shop/services/store.secret.env") {
		t.Fatalf("no reference to the target-side credential:\n%s", doc)
	}
}

// A workload that needs a service must be able to reach it and to know how.
func TestNeedingAServiceJoinsItAndReadsItsURL(t *testing.T) {
	r, err := serviceSpec(t, "services: {store: {driver: postgres, version: 17}}\n").Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out.Bytes)
	if !strings.Contains(body, "ob_shop") {
		t.Fatalf("workload did not join the service network:\n%s", body)
	}
	if !strings.Contains(body, "/var/lib/ob/shop/services/store.client.env") {
		t.Fatalf("workload cannot learn how to reach the service:\n%s", body)
	}
	// depends_on cannot cross Compose projects; emitting it would make the
	// runtime unstartable.
	if strings.Contains(body, "depends_on") {
		t.Fatalf("depends_on must not name a service in another project:\n%s", body)
	}
}

// Guessing an image from an identifier would produce a container that starts
// and stores nothing durable.
func TestUnknownDriverIsRefusedWithAlternatives(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: shop
environments: {production: {server: root@h}}
workloads: {web: {role: application, image: x:1}}
services: {store: {driver: cockroach, version: 24}}
`), "ob.yml")
	if err == nil {
		t.Fatal("an unknown driver must be refused")
	}
	if !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "daemon workload") {
		t.Fatalf("the refusal must list what is available and what to do instead: %v", err)
	}
}

// Accepting a setting the driver has no way to read would be worse than
// refusing it: the author would believe it applied.
func TestSettingsRefusedWhereTheyCannotBeApplied(t *testing.T) {
	_, err := serviceSpec(t, "services: {store: {driver: mysql, version: 8, settings: {max_connections: 200}}}\n").
		Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := serviceSpec(t, "services: {store: {driver: mysql, version: 8, settings: {max_connections: 200}}}\n").
		Resolve("production")
	if _, err := r.RenderServices("production"); err == nil {
		t.Fatal("a setting the driver cannot apply must be refused")
	}
}

// Postgres reads settings as server arguments, not environment variables.
func TestPostgresSettingsBecomeServerArguments(t *testing.T) {
	doc := renderStore(t, "services: {store: {driver: postgres, version: 17, settings: {max_connections: 200}}}\n")
	if !strings.Contains(doc, "max_connections=200") || !strings.Contains(doc, "- -c") {
		t.Fatalf("settings did not reach the server:\n%s", doc)
	}
}

// A URL with an empty username fails Redis AUTH outright: Redis 6+
// authenticates the built-in `default` user. This was found by deploying.
func TestRedisURLNamesTheDefaultUser(t *testing.T) {
	spec := serviceSpec(t, "services: {store: {driver: redis, version: \"7.4\"}}\n")
	client, ok := spec.ClientEnvFor("store")
	if !ok {
		t.Fatal("redis has no client contract")
	}
	script := client.ClientEnvScript("/s.env", "/c.env", nil)
	if !strings.Contains(script, "redis://default:$pw@store:6379") {
		t.Fatalf("redis URL must name the default user:\n%s", script)
	}
}

// Most database images want a host and a password separately. Handing an
// application only a URL and telling it to split a string in a shell is not
// managing anything.
func TestConnectionFileCarriesPartsAsWellAsURL(t *testing.T) {
	spec := serviceSpec(t, "services: {store: {driver: postgres, version: 17}}\n")
	client, _ := spec.ClientEnvFor("store")
	script := client.ClientEnvScript("/s.env", "/c.env", nil)
	for _, want := range []string{"STORE_URL", "STORE_HOST", "STORE_PORT", "STORE_USER", "STORE_DATABASE", "STORE_PASSWORD"} {
		if !strings.Contains(script, want) {
			t.Errorf("connection file is missing %s:\n%s", want, script)
		}
	}
	// The password is read back when it exists, never regenerated: an
	// application holding a credential its database has forgotten is a worse
	// outage than any this would prevent.
	if !strings.Contains(script, "if [ -s '/s.env' ]; then") || !strings.Contains(script, "sed -n 's/^POSTGRES_PASSWORD=//p'") {
		t.Errorf("an established credential is not reused:\n%s", script)
	}
}

// Compose interpolates the file it reads. A `$` Onebox generates must survive
// to the container, or a Redis runs with `--requirepass ""` while the
// application holds a real password — healthy, and broken.
func TestGeneratedDollarsSurviveComposeInterpolation(t *testing.T) {
	doc := renderStore(t, "services: {store: {driver: redis, version: \"7.4\"}}\n")
	if !strings.Contains(doc, `--requirepass "$$REDIS_PASSWORD"`) {
		t.Fatalf("a generated $ was not escaped for Compose:\n%s", doc)
	}
	r, err := serviceSpec(t, "services: {store: {driver: redis, version: \"7.4\"}}\n").Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out.Bytes), "$SOMETHING") {
		t.Fatal("an unescaped $ reached the application runtime")
	}
}

// Every application names its own variables. Without a mapping a managed
// service is only usable by one that happens to read the names Onebox chose,
// which almost none do.
func TestAWorkloadCanNameTheConnectionItself(t *testing.T) {
	spec, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: n8n
environments: {production: {server: root@h}}
workloads:
  n8n:
    role: application
    image: n8n:1
    needs:
      - name: store
        env:
          DB_POSTGRESDB_HOST: host
          DB_POSTGRESDB_USER: user
          DB_POSTGRESDB_PASSWORD: password
services: {store: {driver: postgres, version: 16}}
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	client, _ := spec.ClientEnvFor("store")
	n := spec.NamesFor("production")
	script := client.ClientEnvScript(n.ServiceSecretFile("store"), n.ServiceClientFile("store"),
		[]AliasFile{{Path: n.ServiceAliasFile("store", "n8n"), Vars: spec.Workloads["n8n"].Needs[0].Env}})

	for _, want := range []string{"DB_POSTGRESDB_HOST", "DB_POSTGRESDB_USER", "DB_POSTGRESDB_PASSWORD"} {
		if !strings.Contains(script, want) {
			t.Errorf("the workload's own name %s never reaches the target:\n%s", want, script)
		}
	}
	// The password is still only ever a variable on the target.
	if strings.Contains(script, "DB_POSTGRESDB_PASSWORD=\"postgres") {
		t.Error("a credential was materialised outside the target")
	}

	r, err := spec.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Bytes), "store.n8n.env") {
		t.Fatalf("the workload does not read the file it asked for:\n%s", out.Bytes)
	}
}

// A part the driver does not have must not be written as an empty variable: an
// application cannot tell that from a value.
func TestAMappingToAMissingPartIsSkipped(t *testing.T) {
	spec := serviceSpec(t, "services: {store: {driver: redis, version: \"7.4\"}}\n")
	client, _ := spec.ClientEnvFor("store")
	script := client.ClientEnvScript("/s.env", "/c.env",
		[]AliasFile{{Path: "/a.env", Vars: map[string]string{"CACHE_DB": "database"}}})
	if strings.Contains(script, "CACHE_DB") {
		t.Errorf("redis has no database; the variable must be omitted, not empty:\n%s", script)
	}
}

// A driver that has a database puts it in the URL, whatever its scheme.
// ClickHouse speaks HTTP, and keying the database off the scheme handed
// applications a connection string with no database selected — which fails
// only once something queries it.
func TestEveryDriverWithADatabasePutsItInTheURL(t *testing.T) {
	for _, tt := range []struct{ driver, want string }{
		{"postgres", "/shop"},
		{"mysql", "/shop"},
		{"mariadb", "/shop"},
		{"mongodb", "/shop"},
		{"clickhouse", "/shop"},
	} {
		spec := serviceSpec(t, "services: {store: {driver: "+tt.driver+", version: \"1\"}}\n")
		client, ok := spec.ClientEnvFor("store")
		if !ok {
			t.Errorf("%s: no client contract", tt.driver)
			continue
		}
		script := client.ClientEnvScript("/s.env", "/c.env", nil)
		// The database is the last path segment, so it ends the URL or is
		// followed by a query — mongodb needs authSource to authenticate at
		// all, and pinning to the closing quote would forbid it.
		if !strings.Contains(script, tt.want+"\"") && !strings.Contains(script, tt.want+"?") {
			t.Errorf("%s: the URL selects no database:\n%s", tt.driver, script)
		}
	}
	// And a driver with none does not invent one.
	for _, driver := range []string{"redis", "valkey", "rabbitmq", "meilisearch", "nats"} {
		spec := serviceSpec(t, "services: {store: {driver: "+driver+", version: \"1\"}}\n")
		client, _ := spec.ClientEnvFor("store")
		script := client.ClientEnvScript("/s.env", "/c.env", nil)
		if strings.Contains(script, "/shop\"") || strings.Contains(script, "/shop?") {
			t.Errorf("%s has no database and must not name one", driver)
		}
	}
}

// A driver with no credential variables still gets a credential file, because
// the file is the service's Compose env_file. Asserted by running the script,
// not by matching its spelling: any correct implementation must leave the file
// present and mode 0600.
func TestCredentialFileExistsEvenForADriverWithNoSecretVariables(t *testing.T) {
	spec := serviceSpec(t, "services: {store: {driver: nats, version: 2}}\n")
	client, ok := spec.ClientEnvFor("store")
	if !ok {
		t.Fatal("nats service is not declared")
	}
	if len(client.SecretVars) != 0 {
		t.Fatalf("nats gained credential variables; this test no longer covers the zero-variable driver: %v", client.SecretVars)
	}
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "s.env")
	script := client.ClientEnvScript(secretFile, filepath.Join(dir, "c.env"), nil)
	// /bin/sh, deliberately: the emitted script must work under dash, where a
	// redirection failure on a special builtin aborts before `||` can run.
	if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("apply script failed: %v\n%s\n%s", err, out, script)
	}
	info, err := os.Stat(secretFile)
	if err != nil {
		t.Fatalf("credential file absent after a successful apply: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %v, want 0600", info.Mode().Perm())
	}
}

// The credential file is the only copy of a password the database already
// holds. A truncate-then-append cannot be made safe with `||` guards: the
// truncation lands first, so a failure between the two leaves an empty or
// half-written file that the *next* apply reads back — either minting a fresh
// password the database will reject, or canonicalising a truncated one. The
// write must be atomic, which means a temp file and a rename.
func TestCredentialWritesAreAtomic(t *testing.T) {
	client := ClientEnv{
		Prefix: "PG", Host: "pg", Port: 5432, Scheme: "postgres",
		SecretVars: []string{"POSTGRES_PASSWORD"}, User: "onebox", Database: "app",
	}
	script := client.ClientEnvScript("/s.env", "/c.env", []AliasFile{{Path: "/a.env", Vars: map[string]string{"DB_URL": "url"}}})

	for _, target := range []string{"'/s.env'", "'/c.env'", "'/a.env'"} {
		if strings.Contains(script, ": > "+target) {
			t.Errorf("%s is truncated in place; a failed write destroys it:\n%s", target, script)
		}
		if !strings.Contains(script, "mv -f") {
			t.Errorf("%s is not installed by rename:\n%s", target, script)
		}
	}
	// Nothing may read a target back between truncation and completion, and no
	// `: >` should survive anywhere in the script.
	if strings.Contains(script, ": > ") {
		t.Errorf("script still truncates a live file in place:\n%s", script)
	}
}

// Redis answers PONG while refusing every write when a background save has
// failed and stop-writes-on-bgsave-error is enabled — its own default. A
// connection-only probe therefore reports the service healthy exactly when the
// behaviour callers depend on is unavailable, and a health-gated rollout
// converges onto a dependency that cannot store anything.
func TestRedisFamilyHealthChecksProveAWrite(t *testing.T) {
	rendered := renderServices(t, `api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1}
services:
  redis: {version: 8-alpine}
  valkey: {version: 8-alpine}
`)
	for _, driver := range []string{"redis", "valkey"} {
		doc := string(rendered[driver])
		if !strings.Contains(doc, "set ob:health") {
			t.Fatalf("%s health check does not write:\n%s", driver, doc)
		}
		// A PING-only probe is the regression this guards.
		if strings.Contains(doc, "ping | grep") {
			t.Fatalf("%s health check regressed to a connection-only ping:\n%s", driver, doc)
		}
		// Bounded, so the probe cannot accumulate keys.
		if !strings.Contains(doc, "EX 30") {
			t.Fatalf("%s health-check key has no TTL:\n%s", driver, doc)
		}
		// The generated credential still reaches the container unexpanded on the
		// host: Compose reads `$$` and passes `$`.
		if !strings.Contains(doc, `-a "$$REDIS_PASSWORD"`) {
			t.Fatalf("%s health check lost its escaped credential reference:\n%s", driver, doc)
		}
		// No bare `$` of our own, which Compose would interpolate away.
		probe := doc[strings.Index(doc, driver+"-cli"):]
		probe = probe[:strings.Index(probe, "\n")]
		if strings.Count(probe, "$")-strings.Count(probe, "$$")*2 != 0 {
			t.Fatalf("%s health check carries an unescaped dollar: %s", driver, probe)
		}
	}
}

func renderServices(t *testing.T, src string) map[string][]byte {
	t.Helper()
	spec, err := LoadBytes([]byte(src), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := spec.Render("production", "rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.Services
}

// persistence.mode was declared, validated, defaulted — and then ignored by
// every managed service. `mode: ephemeral` rendered the same durable volume as
// a service that declared nothing, and redis and valkey additionally fsynced
// every write into it through a hardcoded --appendonly yes.
func TestEphemeralServicesOwnNoDurableVolume(t *testing.T) {
	for _, svc := range []string{
		"postgres", "mysql", "mariadb", "mongodb", "clickhouse",
		"redis", "valkey", "rabbitmq", "meilisearch", "nats",
	} {
		version := map[string]string{
			"postgres": "17", "mysql": "8.0", "mariadb": "11.4", "mongodb": "8.0",
			"clickhouse": "25.3", "redis": "8-alpine", "valkey": "8-alpine",
			"rabbitmq": "4", "meilisearch": "1.10", "nats": "2.10",
		}[svc]
		rendered := renderServices(t, "api_version: onebox.run/v1\napp: sample\n"+
			"environments: {production: {server: root@h}}\nworkloads:\n  web: {role: application, image: x:1}\n"+
			"services:\n  "+svc+":\n    version: \""+version+"\"\n    persistence: {mode: ephemeral}\n")
		if strings.Contains(string(rendered[svc]), "_"+svc+"_data") {
			t.Fatalf("%s declared ephemeral still owns a durable volume:\n%s", svc, rendered[svc])
		}
	}
}

// Omitted persistence stays durable, so a project that never declared it keeps
// the volume and the append-only log it has always had.
func TestDurableRedisKeepsItsVolumeAndAppendOnlyLog(t *testing.T) {
	for _, decl := range []string{
		"  redis: {version: 8-alpine}\n",
		"  redis: {version: 8-alpine, persistence: {mode: durable}}\n",
	} {
		rendered := renderServices(t, "api_version: onebox.run/v1\napp: sample\n"+
			"environments: {production: {server: root@h}}\nworkloads:\n  web: {role: application, image: x:1}\nservices:\n"+decl)
		doc := string(rendered["redis"])
		if !strings.Contains(doc, "_redis_data") {
			t.Fatalf("durable redis lost its volume:\n%s", doc)
		}
		if !strings.Contains(doc, `--appendonly "yes"`) {
			t.Fatalf("durable redis lost its append-only log:\n%s", doc)
		}
	}
}

// A disposable cache should not pay an fsync per write, nor grow a log nothing
// intends to read back.
func TestEphemeralRedisFamilyDisablesBothPersistenceMechanisms(t *testing.T) {
	for _, svc := range []string{"redis", "valkey"} {
		rendered := renderServices(t, "api_version: onebox.run/v1\napp: sample\n"+
			"environments: {production: {server: root@h}}\nworkloads:\n  web: {role: application, image: x:1}\n"+
			"services:\n  "+svc+": {version: 8-alpine, persistence: {mode: ephemeral}}\n")
		doc := string(rendered[svc])
		if !strings.Contains(doc, `--appendonly "no"`) {
			t.Fatalf("ephemeral %s still runs the append-only log:\n%s", svc, doc)
		}
		if !strings.Contains(doc, `--save ""`) {
			t.Fatalf("ephemeral %s still snapshots:\n%s", svc, doc)
		}
	}
}

// An authored setting replaces the mode's default rather than being appended
// beside it. Appending produced `--appendonly yes --appendonly no`, which is
// what made an author compensate for the driver in the first place.
func TestAuthoredSettingOverridesTheModeDefaultExactlyOnce(t *testing.T) {
	rendered := renderServices(t, `api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1}
services:
  redis:
    version: 8-alpine
    persistence: {mode: ephemeral}
    settings: {appendonly: "yes"}
`)
	doc := string(rendered["redis"])
	if n := strings.Count(doc, "--appendonly"); n != 1 {
		t.Fatalf("--appendonly appears %d times, want exactly 1:\n%s", n, doc)
	}
	if !strings.Contains(doc, `--appendonly "yes"`) {
		t.Fatalf("the authored setting did not win:\n%s", doc)
	}
	// The mode still owns storage: an explicit server flag does not buy a volume.
	if strings.Contains(doc, "_redis_data") {
		t.Fatalf("a driver setting overrode the declared data lifetime:\n%s", doc)
	}
}

// Only ephemeral turns persistence off. `external` means the operator covers
// this data, and a mode table that did not know the word rendered a durable
// volume with the append-only log silently switched off — a durability
// downgrade on the one mode that says the data matters.
func TestOnlyEphemeralDisablesServerPersistence(t *testing.T) {
	for _, mode := range []string{"durable", "external"} {
		rendered := renderServices(t, "api_version: onebox.run/v1\napp: sample\n"+
			"environments: {production: {server: root@h}}\nworkloads:\n  web: {role: application, image: x:1}\n"+
			"services:\n  redis: {version: 8-alpine, persistence: {mode: "+mode+"}}\n")
		doc := string(rendered["redis"])
		if !strings.Contains(doc, `--appendonly "yes"`) {
			t.Fatalf("mode %q disabled the append-only log:\n%s", mode, doc)
		}
		if !strings.Contains(doc, "_redis_data") {
			t.Fatalf("mode %q lost its volume:\n%s", mode, doc)
		}
	}
	// Every value the grammar permits is covered above or by the ephemeral
	// tests, so a new mode cannot be added without this failing.
	if len(ePersistence) != 3 {
		t.Fatalf("persistence modes changed to %v; extend these tests before shipping", ePersistence)
	}
}

// An ephemeral service owns no durable volume. A volume the author names would
// still reach the canonical form, Spec.All's preflight collision check, and the
// protected-identity record, while nothing ever created or mounted it — the
// declaration would be silently ignored rather than refused.
func TestEphemeralServiceCannotDeclareVolumes(t *testing.T) {
	src := `api_version: onebox.run/v1
app: sample
environments: {production: {server: root@h}}
workloads:
  web: {role: application, image: x:1}
services:
  redis: {version: 8-alpine, persistence: {mode: ephemeral}, volumes: [cache]}
`
	_, err := LoadBytes([]byte(src), "ob.yml")
	if err == nil {
		t.Fatal("an ephemeral service declaring volumes was accepted")
	}
	if !strings.Contains(err.Error(), "owns no durable volume") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}
}

// Protection is a contract about recovering durable data. With no volume
// rendered, seeding the active volume fails at apply time against one that was
// never created, and the sealed identity names it anyway.
func TestEphemeralServiceCannotDeclareProtection(t *testing.T) {
	src := strings.Replace(validProtectionProject,
		"  postgres:\n    version: 17\n    protection:",
		"  postgres:\n    version: 17\n    persistence: {mode: ephemeral}\n    protection:", 1)
	_, err := LoadBytes([]byte(src), "ob.yml")
	if err == nil {
		t.Fatal("an ephemeral service declaring protection was accepted")
	}
	if !strings.Contains(err.Error(), "no durable data to protect") {
		t.Fatalf("refusal does not explain itself: %v", err)
	}
}
