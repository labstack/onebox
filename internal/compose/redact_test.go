package compose

import (
	"strings"
	"testing"
)

const renderedSample = `name: demo
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: supersecret
      POSTGRES_USER: postgres
      AWS_SECRET_ACCESS_KEY: "akiaLIVEsecret/value+with=chars"
  server:
    image: app:1
    env_file:
      - path: ./.env
        required: true
    environment:
      DATABASE_URL: postgresql://postgres:supersecret@postgres:5432/monk
      EMPTY_ONE: null
`

func TestRedactEnvYAMLHidesValuesKeepsKeys(t *testing.T) {
	out, err := RedactEnvYAML([]byte(renderedSample))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// No secret VALUE may survive.
	for _, secret := range []string{"supersecret", "akiaLIVEsecret", "postgresql://postgres"} {
		if strings.Contains(s, secret) {
			t.Fatalf("secret value %q leaked through redaction:\n%s", secret, s)
		}
	}
	// Keys must survive — the diff stays structurally reviewable.
	for _, key := range []string{"POSTGRES_PASSWORD", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "POSTGRES_USER"} {
		if !strings.Contains(s, key) {
			t.Fatalf("env key %q was lost:\n%s", key, s)
		}
	}
	// Non-environment content (image, env_file path) is untouched.
	if !strings.Contains(s, "postgres:16") || !strings.Contains(s, "./.env") {
		t.Fatalf("non-env content must be preserved:\n%s", s)
	}
	// The placeholder is a content hash.
	if !strings.Contains(s, "redacted:sha256:") {
		t.Fatalf("expected content-hash placeholder:\n%s", s)
	}
}

func TestRedactEnvYAMLDeterministicAndValueSensitive(t *testing.T) {
	a, err := RedactEnvYAML([]byte(renderedSample))
	if err != nil {
		t.Fatal(err)
	}
	b, err := RedactEnvYAML([]byte(renderedSample))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("redaction must be deterministic")
	}
	// A changed secret VALUE must change its hash (so drift/rotation shows up).
	changed := strings.Replace(renderedSample, "supersecret", "rotatedsecret", 1)
	c, err := RedactEnvYAML([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	if string(c) == string(a) {
		t.Fatal("a changed env value must change its redacted hash")
	}
}
