package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

var ErrActivationCheckpointMissing = errors.New("activation checkpoint missing")

type ActivationPhase string

const (
	ActivationSchemaVersion = "onebox.run/activation-checkpoint/v1alpha1"

	ActivationPrepared              ActivationPhase = "prepared"
	ActivationVerified              ActivationPhase = "verified"
	ActivationSymlinkSwitched       ActivationPhase = "symlink_switched"
	ActivationServingRecorded       ActivationPhase = "serving_recorded"
	ActivationPredecessorSuperseded ActivationPhase = "predecessor_superseded"
)

// ActivationCheckpoint records the last durable boundary of the only
// multi-file release-store mutation. It is deliberately a single per-app file:
// the application lock permits exactly one activation at a time.
type ActivationCheckpoint struct {
	SchemaVersion string          `json:"schema_version"`
	ReleaseID     string          `json:"release_id"`
	Predecessor   string          `json:"predecessor,omitempty"`
	Phase         ActivationPhase `json:"phase"`
	UpdatedAt     string          `json:"updated_at"`
}

func NewActivationCheckpoint(releaseID, predecessor string, phase ActivationPhase, at time.Time) (ActivationCheckpoint, error) {
	checkpoint := ActivationCheckpoint{
		SchemaVersion: ActivationSchemaVersion,
		ReleaseID:     releaseID,
		Predecessor:   predecessor,
		Phase:         phase,
		UpdatedAt:     timestamp(at),
	}
	if err := checkpoint.Validate(); err != nil {
		return ActivationCheckpoint{}, err
	}
	return checkpoint, nil
}

func (checkpoint *ActivationCheckpoint) Advance(phase ActivationPhase, at time.Time) error {
	if checkpoint == nil {
		return fmt.Errorf("activation checkpoint is nil")
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if activationPhaseIndex(phase) != activationPhaseIndex(checkpoint.Phase)+1 {
		return fmt.Errorf("activation checkpoint transition %s -> %s is invalid", checkpoint.Phase, phase)
	}
	previous, _ := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt)
	if at.UTC().Before(previous) {
		return fmt.Errorf("activation checkpoint timestamp moved backwards")
	}
	checkpoint.Phase = phase
	checkpoint.UpdatedAt = timestamp(at)
	return checkpoint.Validate()
}

func (checkpoint ActivationCheckpoint) Validate() error {
	if checkpoint.SchemaVersion != ActivationSchemaVersion {
		return fmt.Errorf("activation checkpoint schema %q is not supported", checkpoint.SchemaVersion)
	}
	if !IsID(checkpoint.ReleaseID) {
		return fmt.Errorf("activation checkpoint release id %q is invalid", checkpoint.ReleaseID)
	}
	if checkpoint.Predecessor != "" && (!IsID(checkpoint.Predecessor) || checkpoint.Predecessor == checkpoint.ReleaseID) {
		return fmt.Errorf("activation checkpoint predecessor %q is invalid", checkpoint.Predecessor)
	}
	if activationPhaseIndex(checkpoint.Phase) < 0 {
		return fmt.Errorf("activation checkpoint phase %q is invalid", checkpoint.Phase)
	}
	if _, err := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt); err != nil {
		return fmt.Errorf("activation checkpoint timestamp is invalid")
	}
	return nil
}

func activationPhaseIndex(phase ActivationPhase) int {
	switch phase {
	case ActivationPrepared:
		return 0
	case ActivationVerified:
		return 1
	case ActivationSymlinkSwitched:
		return 2
	case ActivationServingRecorded:
		return 3
	case ActivationPredecessorSuperseded:
		return 4
	default:
		return -1
	}
}

func EncodeActivationCheckpoint(checkpoint ActivationCheckpoint) ([]byte, error) {
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode activation checkpoint: %w", err)
	}
	return append(body, '\n'), nil
}

func DecodeActivationCheckpoint(body []byte) (ActivationCheckpoint, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var checkpoint ActivationCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return ActivationCheckpoint{}, fmt.Errorf("activation checkpoint is not valid closed JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ActivationCheckpoint{}, fmt.Errorf("activation checkpoint contains trailing data")
	}
	if err := checkpoint.Validate(); err != nil {
		return ActivationCheckpoint{}, err
	}
	return checkpoint, nil
}

func ActivationCheckpointPath(n app.Names) string {
	return PathsFor(n).Base + "/activation.json"
}

func ActivationCheckpointWrite(n app.Names, checkpoint ActivationCheckpoint) (command, input string, err error) {
	body, err := EncodeActivationCheckpoint(checkpoint)
	if err != nil {
		return "", "", err
	}
	path := ActivationCheckpointPath(n)
	template := path + ".tmp.XXXXXX"
	command = "set -eu; test -d " + q(PathsFor(n).Base) + "; umask 077; tmp=$(mktemp " + q(template) + "); " +
		`trap 'rm -f "$tmp"' 0 1 2 15; cat > "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" ` + q(path) + `; trap - 0 1 2 15`
	return command, string(body), nil
}

func ReadActivationCheckpoint(ctx context.Context, target transport.Transport, n app.Names) (ActivationCheckpoint, error) {
	path := ActivationCheckpointPath(n)
	command := `if [ ! -e ` + q(path) + ` ]; then exit 3; fi; ` +
		`if [ ! -f ` + q(path) + ` ]; then exit 4; fi; ` +
		`mode=$(stat -c '%a' ` + q(path) + ` 2>/dev/null || stat -f '%Lp' ` + q(path) + ` 2>/dev/null) || exit 5; ` +
		`printf 'mode=%s\n' "$mode"; cat ` + q(path)
	result, err := target.Run(ctx, command)
	if err != nil {
		return ActivationCheckpoint{}, err
	}
	switch result.ExitCode {
	case 0:
	case 3:
		return ActivationCheckpoint{}, ErrActivationCheckpointMissing
	case 4:
		return ActivationCheckpoint{}, fmt.Errorf("activation checkpoint is not a regular file")
	default:
		return ActivationCheckpoint{}, fmt.Errorf("activation checkpoint read failed: %s", strings.TrimSpace(result.Stderr))
	}
	mode, body, found := strings.Cut(result.Stdout, "\n")
	if !found || mode != "mode=600" {
		return ActivationCheckpoint{}, fmt.Errorf("activation checkpoint must have mode 0600")
	}
	return DecodeActivationCheckpoint([]byte(body))
}
