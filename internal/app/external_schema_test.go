package app

import (
	"strings"
	"testing"
)

const validExternalServiceProject = `api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@app.example.net}}
workloads:
  web:
    image: nginx:1
    needs:
      - name: database
        condition: healthy
        env: {DATABASE_URL: url}
external_services:
  database:
    driver: postgres
    connection:
      source: {file: secrets/database.env, provider: sops}
      entries: {url: DATABASE_URL}
    protection_owner: platform-team/rds
    probe: {}
`

func TestExternalServiceFixtures(t *testing.T) {
	fixtures := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "external_service_valid",
			yaml: validExternalServiceProject,
		},
		{
			name: "external_service_ambiguous_owner",
			yaml: `api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@app.example.net}}
workloads: {web: {image: nginx:1}}
services: {database: {driver: postgres, version: 17}}
external_services:
  database:
    driver: postgres
    connection:
      source: {file: secrets/database.env, provider: sops}
      entries: {url: DATABASE_URL}
    protection_owner: platform-team/rds
`,
			code: "identifier_collision",
		},
		{
			name: "external_service_lifecycle_field_refused",
			yaml: `api_version: onebox.run/v1
app: shop
environments: {production: {server: deploy@app.example.net}}
workloads: {web: {image: nginx:1}}
external_services:
  database:
    driver: postgres
    version: 17
    connection:
      source: {file: secrets/database.env, provider: sops}
      entries: {url: DATABASE_URL}
    protection_owner: platform-team/rds
`,
			code: "unknown_field",
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			project, err := LoadBytes([]byte(fixture.yaml), fixture.name+".yml")
			if fixture.code != "" {
				assertAppErrorCode(t, err, fixture.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			external := project.ExternalServices["database"]
			if external.Driver != "postgres" || external.ProtectionOwner != "platform-team/rds" {
				t.Fatalf("external service = %#v", external)
			}
			if external.Connection.Source.Provider != "sops" || external.Connection.Entries["url"] != "DATABASE_URL" {
				t.Fatalf("trusted connection = %#v", external.Connection)
			}
			if external.Probe == nil || external.Probe.Kind != "driver-health" || external.Probe.Timeout != "5s" || external.Probe.MaximumAge != "5m" {
				t.Fatalf("read-only probe defaults = %#v", external.Probe)
			}
		})
	}
}

func TestExternalNeedMustMapADeclaredTrustedEntry(t *testing.T) {
	project := strings.Replace(validExternalServiceProject, "env: {DATABASE_URL: url}", "env: {DATABASE_HOST: host}", 1)
	_, err := LoadBytes([]byte(project), "ob.yml")
	assertAppErrorCode(t, err, "project_invalid")
}
