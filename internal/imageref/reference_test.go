package imageref

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	valid := []string{
		"nginx",
		"nginx:1.27",
		"ghcr.io/acme/app@" + digest,
		"registry.example.com:5000/acme/app:1.2",
		"[2001:db8::1]:5000/acme/app:1.2",
	}
	for _, value := range valid {
		if err := Validate(value); err != nil {
			t.Errorf("Validate(%q): %v", value, err)
		}
	}

	invalid := []string{
		"",
		"nginx:",
		"nginx latest",
		"ghcr.io/Acme/app:1.2",
		"nginx@sha256:short",
	}
	for _, value := range invalid {
		if err := Validate(value); err == nil {
			t.Errorf("Validate(%q) succeeded", value)
		}
	}
}

func TestWithDigestPreservesReferenceSpelling(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	tests := map[string]string{
		"nginx:1.27":                              "nginx@" + digest,
		"registry.example.com:5000/acme/app:1.2":  "registry.example.com:5000/acme/app@" + digest,
		"nginx@sha256:" + strings.Repeat("a", 64): "nginx@" + digest,
	}
	for input, want := range tests {
		got, err := WithDigest(input, digest)
		if err != nil {
			t.Fatalf("WithDigest(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("WithDigest(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWithDigestRejectsInvalidDigest(t *testing.T) {
	if _, err := WithDigest("nginx:1.27", "sha256:short"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
}
