package compose

import (
	"bytes"
	"strings"
	"testing"
)

func TestMaskValuesYAMLOmitsEveryScalarValue(t *testing.T) {
	secret := "low-entropy-password"
	rendered := []byte(`# ` + secret + ` in a document comment
services:
  web:
    image: example/web:latest # ` + secret + ` in a line comment
    command: ["sh", "-c", "send ` + secret + `"]
    labels:
      example.token: "` + secret + `"
    environment:
      TOKEN: "` + secret + `"
    healthcheck:
      test: ["CMD-SHELL", "curl https://example.test/?token=` + secret + `"]
`)
	key := bytes.Repeat([]byte{1}, 32)

	masked, err := MaskValuesYAML(rendered, key)
	if err != nil {
		t.Fatal(err)
	}
	view := string(masked)
	for _, value := range []string{secret, "example/web:latest", "send ", "https://example.test"} {
		if strings.Contains(view, value) {
			t.Fatalf("masked YAML exposed %q:\n%s", value, view)
		}
	}
	for _, key := range []string{"services:", "web:", "image:", "command:", "labels:", "example.token:", "environment:", "TOKEN:", "healthcheck:", "test:"} {
		if !strings.Contains(view, key) {
			t.Fatalf("masked YAML lost structural key %q:\n%s", key, view)
		}
	}
	if strings.Count(view, "opaque:hmac-sha256:") < 6 {
		t.Fatalf("expected opaque value markers:\n%s", view)
	}

	again, err := MaskValuesYAML(rendered, key)
	if err != nil || !bytes.Equal(masked, again) {
		t.Fatalf("same proposal key must be deterministic: err=%v\n%s\n%s", err, masked, again)
	}
	other, err := MaskValuesYAML(rendered, bytes.Repeat([]byte{2}, 32))
	if err != nil || bytes.Equal(masked, other) {
		t.Fatalf("different proposal keys must not be linkable: err=%v", err)
	}
}

func TestMaskValuesYAMLRejectsWeakKey(t *testing.T) {
	if _, err := MaskValuesYAML([]byte("value: secret\n"), []byte("short")); err == nil {
		t.Fatal("expected weak mask key rejection")
	}
}
