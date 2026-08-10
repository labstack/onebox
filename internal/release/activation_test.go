package release

import (
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

func TestActivationCheckpointRequiresOrderedDurablePhases(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	checkpoint, err := NewActivationCheckpoint(
		"20260809-120000-new", "20260808-120000-old", ActivationPrepared, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{
		ActivationVerified,
		ActivationSymlinkSwitched,
		ActivationServingRecorded,
		ActivationPredecessorSuperseded,
	} {
		at = at.Add(time.Second)
		if err := checkpoint.Advance(phase, at); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}
	if checkpoint.Phase != ActivationPredecessorSuperseded {
		t.Fatalf("phase = %q", checkpoint.Phase)
	}
	if err := checkpoint.Advance(ActivationVerified, at.Add(time.Second)); err == nil {
		t.Fatal("checkpoint accepted a backward transition")
	}
}

func TestActivationCheckpointIsClosedAndModeProtected(t *testing.T) {
	checkpoint, err := NewActivationCheckpoint(
		"20260809-120000-new", "", ActivationPrepared,
		time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeActivationCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(body), "\n}", ",\n  \"extra\": true\n}", 1)
	if _, err := DecodeActivationCheckpoint([]byte(unknown)); err == nil {
		t.Fatal("checkpoint accepted an unknown field")
	}
	command, input, err := ActivationCheckpointWrite(app.Names{App: "sample", BasePath: app.DefaultBasePath}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"umask 077", "chmod 600", "mktemp", "mv -f", "/activation.json"} {
		if !strings.Contains(command, want) {
			t.Errorf("write command missing %q: %s", want, command)
		}
	}
	if input != string(body) {
		t.Fatal("checkpoint writer changed the encoded body")
	}
}
