package release

import (
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func TestSecretCheckpointClosedModeProtectedContract(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	checkpoint, err := NewSecretCheckpoint(
		"20260809-120000-current",
		"sg-111111111111111111111111",
		"sg-222222222222222222222222",
		[]string{"worker", "web", "web"},
		[]string{"b.env", "a.env"},
		at,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.MarkReplaced("web", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	body, err := EncodeSecretCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSecretCheckpoint(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Phase != SecretReplacing || strings.Join(decoded.ReplacedWorkloads, ",") != "web" {
		t.Fatalf("replacement boundary was not retained: %#v", decoded)
	}
	if _, err := DecodeSecretCheckpoint(append(body, []byte(`{"extra":true}`)...)); err == nil {
		t.Fatal("trailing checkpoint data was accepted")
	}
	command, input, err := SecretCheckpointWrite(app.Names{App: "shop", BasePath: "/srv/onebox"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "umask 077") || !strings.Contains(command, "chmod 600") || !strings.Contains(command, "mv -f") {
		t.Fatalf("checkpoint write is not private and atomic: %s", command)
	}
	for _, forbidden := range []string{"TOKEN=", "sha256:", "password"} {
		if strings.Contains(input, forbidden) {
			t.Fatalf("checkpoint leaked forbidden secret material %q: %s", forbidden, input)
		}
	}
}

func TestSecretCheckpointRejectsInvalidGenerationAndReplacement(t *testing.T) {
	checkpoint, err := NewSecretCheckpoint(
		"20260809-120000-current",
		"sg-111111111111111111111111",
		"sg-222222222222222222222222",
		[]string{"web"}, []string{"secret.env"}, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.NewGeneration = "sha256-secret-content"
	if err := checkpoint.Validate(); err == nil {
		t.Fatal("content-derived or malformed generation was accepted")
	}
	checkpoint.NewGeneration = "sg-222222222222222222222222"
	if err := checkpoint.MarkReplaced("database", time.Now().UTC().Add(time.Second)); err == nil {
		t.Fatal("replacement outside the bound workload set was accepted")
	}
}
