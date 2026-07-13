package config

import (
	"strings"
	"testing"
)

const extendedURLVerification = `verification:
  - url: "https://example.test/health?token=secret"
    status_codes: [200, 204]
    required_headers:
      Content-Type: application/json
      X-Release: r42
    json_assertions:
      - { path: service.ready, equals: true }
      - { path: service.replicas, equals: 2 }
      - { path: service.note, equals: null }
      - { path: items.0.name, equals: primary }
`

func TestExtendedURLVerificationLoadsAndRoundTrips(t *testing.T) {
	cfg := loadAndValidateV1(t, stableV1Minimal+extendedURLVerification)
	check := cfg.Verify[0]
	if got := check.StatusCodes; len(got) != 2 || got[0] != 200 || got[1] != 204 {
		t.Fatalf("status_codes = %v", got)
	}
	if got := check.RequiredHeaders["Content-Type"]; got != "application/json" {
		t.Fatalf("required Content-Type = %q", got)
	}
	if got := check.JSONAssertions; len(got) != 4 || got[0].Path != "service.ready" || got[0].Equals != true {
		t.Fatalf("json_assertions = %#v", got)
	}
	if check.JSONAssertions[2].Equals != nil {
		t.Fatalf("null assertion decoded as %#v", check.JSONAssertions[2].Equals)
	}

	encoded, err := cfg.YAML()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadBytes(encoded, "resolved-ob.yml")
	if err != nil {
		t.Fatalf("reload extended verification: %v\n%s", err, encoded)
	}
	if err := reloaded.Validate(); err != nil {
		t.Fatalf("validate reloaded verification: %v", err)
	}
}

func TestExtendedURLVerificationCUERejectsInvalidShapes(t *testing.T) {
	tests := map[string]string{
		"empty status list": `verification:
  - { url: "https://example.test/", status_codes: [] }
`,
		"status below HTTP range": `verification:
  - { url: "https://example.test/", status_codes: [99] }
`,
		"status above HTTP range": `verification:
  - { url: "https://example.test/", status_codes: [600] }
`,
		"invalid header name": `verification:
  - url: "https://example.test/"
    required_headers: { "Bad Header": value }
`,
		"header value newline": `verification:
  - url: "https://example.test/"
    required_headers:
      X-Release: |-
        first
        second
`,
		"empty path segment": `verification:
  - url: "https://example.test/"
    json_assertions: [{ path: service..ready, equals: true }]
`,
		"object equality": `verification:
  - url: "https://example.test/"
    json_assertions: [{ path: service, equals: { ready: true } }]
`,
		"list equality": `verification:
  - url: "https://example.test/"
    json_assertions: [{ path: service, equals: [ready] }]
`,
		"missing equality": `verification:
  - url: "https://example.test/"
    json_assertions: [{ path: service.ready }]
`,
		"URL fields on container check": `verification:
  - { component: web, exec: "true", status_codes: [200] }
`,
	}
	for name, verification := range tests {
		t.Run(name, func(t *testing.T) {
			requireV1Rejected(t, stableV1Minimal+verification)
		})
	}
}

func TestExtendedURLVerificationGoValidation(t *testing.T) {
	tests := map[string]struct {
		check configVerifyCheck
		want  string
	}{
		"duplicate status": {
			check: configVerifyCheck{statusCodes: []int{200, 200}},
			want:  "duplicate status",
		},
		"case-insensitive duplicate header": {
			check: configVerifyCheck{headers: map[string]string{"X-Release": "one", "x-release": "two"}},
			want:  "more than once",
		},
		"header newline": {
			check: configVerifyCheck{headers: map[string]string{"X-Release": "one\ntwo"}},
			want:  "newline",
		},
		"composite equality": {
			check: configVerifyCheck{assertions: []JSONAssertion{{Path: "service", Equals: map[string]any{"ready": true}}}},
			want:  "string, number, boolean, or null",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := loadAndValidateV1(t, stableV1Minimal)
			cfg.Verify = []VerifyCheck{{
				URL:             "https://example.test/",
				StatusCodes:     test.check.statusCodes,
				RequiredHeaders: test.check.headers,
				JSONAssertions:  test.check.assertions,
			}}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestMigrationRevisionVerificationLoadsAndValidates(t *testing.T) {
	cfg := loadAndValidateV1(t, stableV1AllOptions)
	var found *MigrationRevisionAssertion
	for _, check := range cfg.Verify {
		if check.MigrationRevisions != nil {
			found = check.MigrationRevisions
			break
		}
	}
	if found == nil || found.Job != "schema" || found.Provider != "atlas" || len(found.AppliedRevisions) != 2 {
		t.Fatalf("migration revision assertion = %+v", found)
	}

	cfg.Verify = []VerifyCheck{{MigrationRevisions: &MigrationRevisionAssertion{
		Job: "schema", Provider: "atlas", AppliedRevisions: []string{"r1", "r1"},
	}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate revision") {
		t.Fatalf("duplicate revision error = %v", err)
	}
}

type configVerifyCheck struct {
	statusCodes []int
	headers     map[string]string
	assertions  []JSONAssertion
}
