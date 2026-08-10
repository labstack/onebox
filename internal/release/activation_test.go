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

func TestActivationCheckpointRequiresOrderedDurablePhases(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	checkpoint, err := NewActivationCheckpoint(
		"20260809-120000-new", "20260808-120000-old", ActivationPrepared, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []ActivationPhase{
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
	before := checkpoint
	if err := checkpoint.Advance(ActivationVerified, at.Add(time.Second)); err == nil {
		t.Fatal("checkpoint accepted a backward transition")
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("rejected transition mutated checkpoint: before=%+v after=%+v", before, checkpoint)
	}
}

func TestActivationCheckpointRefusesSkippedPhaseAndBackwardTime(t *testing.T) {
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	checkpoint, err := NewActivationCheckpoint("20260809-120000-new", "", ActivationPrepared, at)
	if err != nil {
		t.Fatal(err)
	}
	for name, attempt := range map[string]func() error{
		"skipped phase": func() error { return checkpoint.Advance(ActivationSymlinkSwitched, at.Add(time.Second)) },
		"backward time": func() error { return checkpoint.Advance(ActivationVerified, at.Add(-time.Second)) },
	} {
		t.Run(name, func(t *testing.T) {
			before := checkpoint
			if err := attempt(); err == nil {
				t.Fatal("invalid checkpoint advance succeeded")
			}
			if !reflect.DeepEqual(checkpoint, before) {
				t.Fatalf("rejected advance mutated checkpoint: before=%+v after=%+v", before, checkpoint)
			}
		})
	}
	if _, err := NewActivationCheckpoint("20260809-120000-new", "20260809-120000-new", ActivationPrepared, at); err == nil {
		t.Fatal("checkpoint accepted itself as predecessor")
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
	if _, err := DecodeActivationCheckpoint(append(body, []byte(`{"second":true}`)...)); err == nil {
		t.Fatal("checkpoint accepted trailing data")
	}
	command, _, err := ActivationCheckpointWrite(app.Names{App: "sample", BasePath: app.DefaultBasePath}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"umask 077", "chmod 600", "mktemp", "mv -f", "/activation.json"} {
		if !strings.Contains(command, want) {
			t.Errorf("write command missing %q: %s", want, command)
		}
	}
}

func TestReadActivationCheckpointFailsClosed(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
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
			_, err := ReadActivationCheckpoint(context.Background(), fake, names)
			if err == nil || errors.Is(err, ErrActivationCheckpointMissing) != test.wantMissing {
				t.Fatalf("read error = %v, missing=%v", err, test.wantMissing)
			}
		})
	}
}
