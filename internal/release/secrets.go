package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

var ErrSecretCheckpointMissing = errors.New("secret checkpoint missing")

const (
	SecretCheckpointSchemaVersion = "onebox.run/secret-checkpoint/v1alpha1"

	SecretPrepared   = "prepared"
	SecretReplacing  = "replacing"
	SecretVerifying  = "verifying"
	SecretCommitting = "committing"
	SecretCommitted  = "committed"
	SecretRecovering = "recovering"
)

var opaqueSecretGeneration = regexp.MustCompile(`^sg-[0-9a-f]{24}$`)

// SecretCheckpoint is the durable transaction record for one generation-wide
// rotation. It contains identifiers and paths only, never secret values or
// content hashes. A single per-application record is sufficient because the
// application lock serializes all mutations.
type SecretCheckpoint struct {
	SchemaVersion     string   `json:"schema_version"`
	ReleaseID         string   `json:"release_id"`
	OldGeneration     string   `json:"old_generation"`
	NewGeneration     string   `json:"new_generation"`
	Phase             string   `json:"phase"`
	AffectedWorkloads []string `json:"affected_workloads"`
	PayloadPaths      []string `json:"payload_paths"`
	ReplacedWorkloads []string `json:"replaced_workloads,omitempty"`
	UpdatedAt         string   `json:"updated_at"`
}

func NewSecretCheckpoint(releaseID, oldGeneration, newGeneration string, workloads, paths []string, at time.Time) (SecretCheckpoint, error) {
	checkpoint := SecretCheckpoint{
		SchemaVersion: SecretCheckpointSchemaVersion,
		ReleaseID:     releaseID, OldGeneration: oldGeneration, NewGeneration: newGeneration,
		Phase: SecretPrepared, AffectedWorkloads: sortedUnique(workloads), PayloadPaths: sortedUnique(paths),
		UpdatedAt: timestamp(at),
	}
	if err := checkpoint.Validate(); err != nil {
		return SecretCheckpoint{}, err
	}
	return checkpoint, nil
}

func IsSecretGeneration(value string) bool { return opaqueSecretGeneration.MatchString(value) }

func (checkpoint *SecretCheckpoint) SetPhase(phase string, at time.Time) error {
	if checkpoint == nil {
		return errors.New("secret checkpoint is nil")
	}
	if !validSecretPhase(phase) {
		return fmt.Errorf("secret checkpoint phase %q is invalid", phase)
	}
	previous, err := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt)
	if err != nil || at.UTC().Before(previous) {
		return errors.New("secret checkpoint timestamp moved backwards")
	}
	checkpoint.Phase = phase
	checkpoint.UpdatedAt = timestamp(at)
	return checkpoint.Validate()
}

func (checkpoint *SecretCheckpoint) MarkReplaced(workload string, at time.Time) error {
	if checkpoint == nil {
		return errors.New("secret checkpoint is nil")
	}
	if !contains(checkpoint.AffectedWorkloads, workload) {
		return fmt.Errorf("workload %q is outside the secret checkpoint", workload)
	}
	if !contains(checkpoint.ReplacedWorkloads, workload) {
		checkpoint.ReplacedWorkloads = append(checkpoint.ReplacedWorkloads, workload)
		sort.Strings(checkpoint.ReplacedWorkloads)
	}
	return checkpoint.SetPhase(SecretReplacing, at)
}

func (checkpoint SecretCheckpoint) Validate() error {
	if checkpoint.SchemaVersion != SecretCheckpointSchemaVersion {
		return fmt.Errorf("secret checkpoint schema %q is not supported", checkpoint.SchemaVersion)
	}
	if !IsID(checkpoint.ReleaseID) {
		return fmt.Errorf("secret checkpoint release id %q is invalid", checkpoint.ReleaseID)
	}
	if !IsSecretGeneration(checkpoint.OldGeneration) || !IsSecretGeneration(checkpoint.NewGeneration) || checkpoint.OldGeneration == checkpoint.NewGeneration {
		return errors.New("secret checkpoint generations are invalid")
	}
	if !validSecretPhase(checkpoint.Phase) {
		return fmt.Errorf("secret checkpoint phase %q is invalid", checkpoint.Phase)
	}
	if len(checkpoint.PayloadPaths) == 0 {
		return errors.New("secret checkpoint must bind payload paths")
	}
	if !sortedUniqueExact(checkpoint.AffectedWorkloads) || !sortedUniqueExact(checkpoint.PayloadPaths) || !sortedUniqueExact(checkpoint.ReplacedWorkloads) {
		return errors.New("secret checkpoint lists must be sorted and unique")
	}
	for _, workload := range checkpoint.ReplacedWorkloads {
		if !contains(checkpoint.AffectedWorkloads, workload) {
			return fmt.Errorf("replaced workload %q is outside the secret checkpoint", workload)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt); err != nil {
		return errors.New("secret checkpoint timestamp is invalid")
	}
	return nil
}

func validSecretPhase(phase string) bool {
	switch phase {
	case SecretPrepared, SecretReplacing, SecretVerifying, SecretCommitting, SecretCommitted, SecretRecovering:
		return true
	default:
		return false
	}
}

func EncodeSecretCheckpoint(checkpoint SecretCheckpoint) ([]byte, error) {
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode secret checkpoint: %w", err)
	}
	return append(body, '\n'), nil
}

func DecodeSecretCheckpoint(body []byte) (SecretCheckpoint, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var checkpoint SecretCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return SecretCheckpoint{}, errors.New("secret checkpoint is not valid closed JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return SecretCheckpoint{}, errors.New("secret checkpoint contains trailing data")
	}
	if err := checkpoint.Validate(); err != nil {
		return SecretCheckpoint{}, err
	}
	return checkpoint, nil
}

func SecretCheckpointPath(names app.Names) string {
	return PathsFor(names).Base + "/secret-activation.json"
}

func SecretCheckpointWrite(names app.Names, checkpoint SecretCheckpoint) (command, input string, err error) {
	body, err := EncodeSecretCheckpoint(checkpoint)
	if err != nil {
		return "", "", err
	}
	path := SecretCheckpointPath(names)
	command = "set -eu; test -d " + q(PathsFor(names).Base) + "; umask 077; tmp=$(mktemp " + q(path+".tmp.XXXXXX") + "); " +
		`trap 'rm -f "$tmp"' 0 1 2 15; cat > "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" ` + q(path) + `; trap - 0 1 2 15`
	return command, string(body), nil
}

func ReadSecretCheckpoint(ctx context.Context, target transport.Transport, names app.Names) (SecretCheckpoint, error) {
	path := SecretCheckpointPath(names)
	command := `if [ ! -e ` + q(path) + ` ]; then exit 3; fi; ` +
		`if [ ! -f ` + q(path) + ` ]; then exit 4; fi; ` +
		`mode=$(stat -c '%a' ` + q(path) + ` 2>/dev/null || stat -f '%Lp' ` + q(path) + ` 2>/dev/null) || exit 5; ` +
		`printf 'mode=%s\n' "$mode"; cat ` + q(path)
	result, err := target.Run(ctx, command)
	if err != nil {
		return SecretCheckpoint{}, err
	}
	switch result.ExitCode {
	case 0:
	case 3:
		return SecretCheckpoint{}, ErrSecretCheckpointMissing
	case 4:
		return SecretCheckpoint{}, errors.New("secret checkpoint is not a regular file")
	default:
		return SecretCheckpoint{}, fmt.Errorf("secret checkpoint read failed: %s", strings.TrimSpace(result.Stderr))
	}
	mode, body, found := strings.Cut(result.Stdout, "\n")
	if !found || mode != "mode=600" {
		return SecretCheckpoint{}, errors.New("secret checkpoint must have mode 0600")
	}
	return DecodeSecretCheckpoint([]byte(body))
}

func sortedUnique(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	if len(out) == 0 {
		return out
	}
	write := 1
	for read := 1; read < len(out); read++ {
		if out[read] != out[write-1] {
			out[write] = out[read]
			write++
		}
	}
	return out[:write]
}

func sortedUniqueExact(values []string) bool {
	for index, value := range values {
		if strings.TrimSpace(value) == "" || (index > 0 && values[index-1] >= value) {
			return false
		}
	}
	return true
}

func contains(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}
