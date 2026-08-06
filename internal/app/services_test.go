package app

import (
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
