package app

import (
	"errors"
	"strings"
	"testing"
)

func TestPostgresExtensionsSelectTheOneboxImage(t *testing.T) {
	rendered := renderServices(t, `api_version: onebox.run/v1
app: goal
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: goal:1}
services:
  database:
    driver: postgres
    version: 18
    features:
      extensions:
        pg_trgm: {}
        vector: {}
`)
	doc := string(rendered["database"])
	if !strings.Contains(doc, "image: ghcr.io/labstack/onebox-postgres:18\n") {
		t.Fatalf("postgres service does not use the Onebox image:\n%s", doc)
	}
}

func TestProtectedPostgresMustAdoptTheOneboxImageBeforeExtensions(t *testing.T) {
	resolved := serviceImageTestResolved(true)
	service := resolved.Services["database"]
	service.Features = &ServiceFeatures{Extensions: map[string]ServiceExtension{"vector": {}}}
	resolved.Services["database"] = service
	bound, err := resolved.WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"database": {
			BackupState: "enabled", ServiceImage: pinnedServiceImage("postgres", 'a'),
			PublicationVerified: true, DigestAvailable: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bound.RenderServices("production")
	var projectError *Error
	if !errors.As(err, &projectError) || projectError.Code != "service_patch_unsupported" {
		t.Fatalf("protected legacy image error = %v", err)
	}
}

func TestPostgresExtensionsDerivePreloadAndCronSettings(t *testing.T) {
	rendered := renderServices(t, `api_version: onebox.run/v1
app: goal
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: goal:1}
services:
  database:
    driver: postgres
    version: 18
    settings:
      shared_preload_libraries: auto_explain
    features:
      extensions:
        pg_cron: {}
        pgaudit: {}
        pg_stat_statements: {}
`)
	doc := string(rendered["database"])
	for _, setting := range []string{
		"shared_preload_libraries=auto_explain,pg_cron,pg_stat_statements,pgaudit",
		"cron.database_name=goal",
		"cron.use_background_workers=on",
	} {
		if !strings.Contains(doc, setting) {
			t.Errorf("derived setting %q is absent:\n%s", setting, doc)
		}
	}
	if strings.Count(doc, "shared_preload_libraries=") != 1 {
		t.Fatalf("preload setting was rendered more than once:\n%s", doc)
	}
}

func TestServiceExtensionsArePostgresOnly(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: sample:1}
services:
  cache:
    driver: redis
    version: 8
    features: {extensions: {vector: {}}}
`), "ob.yml")
	if err == nil || !strings.Contains(err.Error(), "supported only by the postgres driver") {
		t.Fatalf("non-postgres features error = %v", err)
	}
}

func TestPostgresExtensionsRequireThePublishedImageVersion(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: sample:1}
services:
  postgres:
    version: 17
    features: {extensions: {vector: {}}}
`), "ob.yml")
	if err == nil || !strings.Contains(err.Error(), "require version 18") {
		t.Fatalf("unsupported PostgreSQL version error = %v", err)
	}
}

func TestServiceExtensionNamesAreSafeSQLIdentifiers(t *testing.T) {
	_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: sample:1}
services:
  postgres:
    version: 18
    features:
      extensions:
        vector;drop_table: {}
`), "ob.yml")
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL extension") {
		t.Fatalf("unsafe extension name error = %v", err)
	}
}

func TestPgCronSettingsCannotDisableTheManagedContract(t *testing.T) {
	for _, settings := range []string{
		"cron.database_name: elsewhere",
		"cron.use_background_workers: off",
	} {
		_, err := LoadBytes([]byte(`api_version: onebox.run/v1
app: sample
environments: {production: {server: root@host}}
workloads:
  web: {role: application, image: sample:1}
services:
  postgres:
    version: 18
    settings: {`+settings+`}
    features: {extensions: {pg_cron: {}}}
`), "ob.yml")
		if err == nil || !strings.Contains(err.Error(), "pg_cron") {
			t.Fatalf("settings %q error = %v", settings, err)
		}
	}
}
