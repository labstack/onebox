package release

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
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
	command, _, err := SecretCheckpointWrite(app.Names{App: "shop", BasePath: "/srv/onebox"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "umask 077") || !strings.Contains(command, "chmod 600") || !strings.Contains(command, "mv -f") {
		t.Fatalf("checkpoint write is not private and atomic: %s", command)
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

func TestSecretCheckpointTransitionsAreOrderedAndTransactional(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	checkpoint, err := NewSecretCheckpoint(
		"20260809-120000-current", "sg-111111111111111111111111", "sg-222222222222222222222222",
		[]string{"web"}, []string{"secret.env"}, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	before := checkpoint
	before.AffectedWorkloads = append([]string(nil), checkpoint.AffectedWorkloads...)
	before.PayloadPaths = append([]string(nil), checkpoint.PayloadPaths...)
	before.ReplacedWorkloads = append([]string(nil), checkpoint.ReplacedWorkloads...)
	if err := checkpoint.SetPhase(SecretVerifying, at.Add(time.Second)); err == nil {
		t.Fatal("checkpoint skipped the replacing phase")
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("rejected transition mutated checkpoint: before=%+v after=%+v", before, checkpoint)
	}
	if err := checkpoint.MarkReplaced("web", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetPhase(SecretVerifying, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	before = checkpoint
	before.AffectedWorkloads = append([]string(nil), checkpoint.AffectedWorkloads...)
	before.PayloadPaths = append([]string(nil), checkpoint.PayloadPaths...)
	before.ReplacedWorkloads = append([]string(nil), checkpoint.ReplacedWorkloads...)
	if err := checkpoint.MarkReplaced("web", at.Add(3*time.Second)); err == nil {
		t.Fatal("replacement rewound a verifying checkpoint")
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("rejected replacement mutated checkpoint: before=%+v after=%+v", before, checkpoint)
	}
}

func TestSecretCheckpointRejectsUnsafePayloadPaths(t *testing.T) {
	for _, payloadPath := range []string{"../secret.env", "a/../secret.env", "/secret.env", `a\secret.env`, "."} {
		t.Run(payloadPath, func(t *testing.T) {
			_, err := NewSecretCheckpoint(
				"20260809-120000-current", "sg-111111111111111111111111", "sg-222222222222222222222222",
				[]string{"web"}, []string{payloadPath}, time.Now().UTC(),
			)
			if err == nil {
				t.Fatalf("unsafe payload path %q was accepted", payloadPath)
			}
		})
	}
}

func TestReadSecretCheckpointFailsClosed(t *testing.T) {
	names := app.Names{App: "shop", BasePath: "/srv/onebox"}
	tests := []struct {
		name        string
		result      transport.Result
		wantMissing bool
	}{
		{name: "missing", result: transport.Result{ExitCode: 3}, wantMissing: true},
		{name: "not regular", result: transport.Result{ExitCode: 4}},
		{name: "stat failure", result: transport.Result{ExitCode: 5}},
		{name: "unsafe mode", result: transport.Result{Stdout: "mode=644\n{}"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(string) (transport.Result, bool) { return test.result, true }}
			_, err := ReadSecretCheckpoint(context.Background(), fake, names)
			if err == nil || errors.Is(err, ErrSecretCheckpointMissing) != test.wantMissing {
				t.Fatalf("read error = %v, missing=%v", err, test.wantMissing)
			}
		})
	}
}
