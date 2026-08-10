package release

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const (
	ManifestSchemaVersion = "onebox.run/release-manifest/v1alpha1"
	ManifestFileName      = "manifest.json"

	KindApplication = "application"
	KindBootstrap   = "bootstrap"

	StateStaged     = "staged"
	StateVerified   = "verified"
	StateServing    = "serving"
	StateSuperseded = "superseded"
	StateFailed     = "failed"
	StateAborted    = "aborted"

	OutcomePending   = "pending"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeAborted   = "aborted"
)

// StateTransition is the append-only lifecycle history inside one manifest.
// The current state is duplicated on Manifest so status can inspect it without
// interpreting history; validation requires both views to agree.
type StateTransition struct {
	State string `json:"state"`
	At    string `json:"at"`
}

// Manifest is the durable identity and lifecycle truth for one release-store
// directory. OperationOutcome is deliberately independent from State: a
// post-activation action can fail while the release remains truthfully serving.
type Manifest struct {
	SchemaVersion    string            `json:"schema_version"`
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	State            string            `json:"state"`
	OperationOutcome string            `json:"operation_outcome"`
	OutcomeAt        string            `json:"outcome_at,omitempty"`
	Predecessor      string            `json:"predecessor,omitempty"`
	Transitions      []StateTransition `json:"transitions"`
}

// ManifestError is safe to expose in status and structured command output. It
// identifies the failed evidence without including manifest bytes.
type ManifestError struct {
	Code      string
	ReleaseID string
	Detail    string
}

func (err *ManifestError) Error() string {
	message := err.Code
	if err.ReleaseID != "" {
		message += ": release " + err.ReleaseID
	}
	if err.Detail != "" {
		message += ": " + err.Detail
	}
	return message
}

func NewManifest(id, kind string, at time.Time) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		ID:               id,
		Kind:             kind,
		State:            StateStaged,
		OperationOutcome: OutcomePending,
		Transitions:      []StateTransition{{State: StateStaged, At: timestamp(at)}},
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Transition advances the lifecycle state. A superseded application may be
// activated again by rollback; that is a new activation transition and records
// the release being left as its new predecessor.
func (manifest *Manifest) Transition(next string, at time.Time, predecessor string) error {
	if manifest == nil {
		return manifestInvalid("", "manifest is nil")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if !allowedTransition(manifest.Kind, manifest.State, next) {
		return manifestInvalid(manifest.ID, fmt.Sprintf("state transition %s -> %s is not permitted for %s", manifest.State, next, manifest.Kind))
	}
	last, _ := time.Parse(time.RFC3339Nano, manifest.Transitions[len(manifest.Transitions)-1].At)
	if at.UTC().Before(last) {
		return manifestInvalid(manifest.ID, "transition timestamp precedes the current state")
	}
	if next == StateServing {
		if predecessor != "" && (!IsID(predecessor) || predecessor == manifest.ID) {
			return manifestInvalid(manifest.ID, "serving predecessor is invalid")
		}
		manifest.Predecessor = predecessor
	} else if predecessor != "" && predecessor != manifest.Predecessor {
		return manifestInvalid(manifest.ID, "predecessor may change only during activation")
	}
	manifest.State = next
	if next != StateSuperseded {
		if next == StateServing {
			// Reactivating a superseded release is a new successful activation,
			// not a continuation of the outcome recorded when it last served.
			manifest.OperationOutcome = OutcomeSucceeded
		} else {
			manifest.OperationOutcome = outcomeForState(manifest.Kind, next, manifest.OperationOutcome)
		}
		if manifest.OperationOutcome == OutcomePending {
			manifest.OutcomeAt = ""
		} else {
			manifest.OutcomeAt = timestamp(at)
		}
	}
	manifest.Transitions = append(manifest.Transitions, StateTransition{State: next, At: timestamp(at)})
	return manifest.Validate()
}

// RecordOperationOutcome records a post-activation terminal result without
// rewriting lifecycle truth. A recovered release may already be superseded by
// the time failure/abort finalization completes.
func (manifest *Manifest) RecordOperationOutcome(outcome string, at time.Time) error {
	if manifest == nil {
		return manifestInvalid("", "manifest is nil")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	serving := manifest.State == StateServing
	superseded := manifest.State == StateSuperseded
	allowedOutcome := outcome == OutcomeSucceeded || outcome == OutcomeFailed || superseded && outcome == OutcomeAborted
	if (!serving && !superseded) || !allowedOutcome {
		return manifestInvalid(manifest.ID, "only a serving or superseded release can record a post-activation outcome")
	}
	last, _ := time.Parse(time.RFC3339Nano, manifest.Transitions[len(manifest.Transitions)-1].At)
	if at.UTC().Before(last) {
		return manifestInvalid(manifest.ID, "operation outcome timestamp precedes the current transition")
	}
	manifest.OperationOutcome = outcome
	manifest.OutcomeAt = timestamp(at)
	return manifest.Validate()
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return &ManifestError{Code: "manifest_schema_unknown", ReleaseID: manifest.ID, Detail: fmt.Sprintf("schema %q is not supported", manifest.SchemaVersion)}
	}
	if !IsID(manifest.ID) {
		return manifestInvalid(manifest.ID, "identifier is invalid")
	}
	if manifest.Kind != KindApplication && manifest.Kind != KindBootstrap {
		return manifestInvalid(manifest.ID, fmt.Sprintf("kind %q is invalid", manifest.Kind))
	}
	if len(manifest.Transitions) == 0 || manifest.Transitions[0].State != StateStaged || manifest.State != manifest.Transitions[len(manifest.Transitions)-1].State {
		return manifestInvalid(manifest.ID, "state and transition history do not agree")
	}
	previousState := ""
	var previousAt time.Time
	for i, transition := range manifest.Transitions {
		at, err := time.Parse(time.RFC3339Nano, transition.At)
		if err != nil {
			return manifestInvalid(manifest.ID, fmt.Sprintf("transition %d has an invalid timestamp", i))
		}
		if i > 0 {
			if at.Before(previousAt) {
				return manifestInvalid(manifest.ID, "transition timestamps are not monotonic")
			}
			if !allowedTransition(manifest.Kind, previousState, transition.State) {
				return manifestInvalid(manifest.ID, fmt.Sprintf("transition %s -> %s is invalid", previousState, transition.State))
			}
		}
		previousState, previousAt = transition.State, at
	}
	if manifest.Predecessor != "" && (!IsID(manifest.Predecessor) || manifest.Predecessor == manifest.ID) {
		return manifestInvalid(manifest.ID, "predecessor is invalid")
	}
	if manifest.Kind == KindBootstrap && (manifest.State == StateServing || manifest.State == StateSuperseded || manifest.Predecessor != "") {
		return manifestInvalid(manifest.ID, "bootstrap manifests cannot serve or have predecessors")
	}
	wantOutcome := outcomeForState(manifest.Kind, manifest.State, manifest.OperationOutcome)
	if manifest.State == StateServing {
		if manifest.OperationOutcome != OutcomeSucceeded && manifest.OperationOutcome != OutcomeFailed {
			return manifestInvalid(manifest.ID, "serving outcome must be succeeded or failed")
		}
	} else if manifest.OperationOutcome != wantOutcome {
		return manifestInvalid(manifest.ID, fmt.Sprintf("outcome %q does not match state %q", manifest.OperationOutcome, manifest.State))
	}
	if manifest.OperationOutcome == OutcomePending {
		if manifest.OutcomeAt != "" {
			return manifestInvalid(manifest.ID, "pending operation has an outcome timestamp")
		}
	} else {
		outcomeAt, err := time.Parse(time.RFC3339Nano, manifest.OutcomeAt)
		if err != nil || (manifest.State != StateSuperseded && outcomeAt.Before(previousAt)) {
			return manifestInvalid(manifest.ID, "terminal operation outcome timestamp is invalid")
		}
	}
	return nil
}

func allowedTransition(kind, from, to string) bool {
	if kind == KindBootstrap {
		return (from == StateStaged && (to == StateVerified || to == StateFailed || to == StateAborted))
	}
	switch from {
	case StateStaged:
		return to == StateVerified || to == StateFailed || to == StateAborted
	case StateVerified:
		return to == StateServing || to == StateFailed || to == StateAborted
	case StateServing:
		return to == StateSuperseded
	case StateSuperseded:
		return to == StateServing
	}
	return false
}

func outcomeForState(kind, state, current string) string {
	switch state {
	case StateStaged:
		return OutcomePending
	case StateVerified:
		if kind == KindBootstrap {
			return OutcomeSucceeded
		}
		return OutcomePending
	case StateServing:
		if current == OutcomeFailed {
			return current
		}
		return OutcomeSucceeded
	case StateSuperseded:
		return current
	case StateFailed:
		return OutcomeFailed
	case StateAborted:
		return OutcomeAborted
	}
	return ""
}

func timestamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func manifestInvalid(id, detail string) error {
	return &ManifestError{Code: "manifest_invalid", ReleaseID: id, Detail: detail}
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, manifestInvalid(manifest.ID, "cannot encode manifest")
	}
	return append(body, '\n'), nil
}

func DecodeManifest(body []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, &ManifestError{Code: "manifest_invalid", Detail: "manifest is not valid closed JSON"}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, &ManifestError{Code: "manifest_invalid", ReleaseID: manifest.ID, Detail: "manifest contains trailing data"}
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ManifestPath(n app.Names, id string) string {
	return n.ReleaseDir(id) + "/" + ManifestFileName
}

// ManifestWrite returns the atomic, mode-protected remote write operation and
// its stdin. Engines can place the command behind their fence; WriteManifest is
// the direct convenience used by stores that already own mutation authority.
func ManifestWrite(n app.Names, manifest Manifest) (command, input string, err error) {
	body, err := EncodeManifest(manifest)
	if err != nil {
		return "", "", err
	}
	dir := n.ReleaseDir(manifest.ID)
	path := ManifestPath(n, manifest.ID)
	template := path + ".tmp.XXXXXX"
	command = "set -eu; test -d " + q(dir) + "; umask 077; tmp=$(mktemp " + q(template) + "); " +
		`trap 'rm -f "$tmp"' 0 1 2 15; cat > "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" ` + q(path) + `; trap - 0 1 2 15`
	return command, string(body), nil
}

func WriteManifest(ctx context.Context, target transport.Transport, n app.Names, manifest Manifest) error {
	command, input, err := ManifestWrite(n, manifest)
	if err != nil {
		return err
	}
	result, err := target.RunInput(ctx, command, input)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return &ManifestError{Code: "manifest_write_failed", ReleaseID: manifest.ID, Detail: detail}
	}
	return nil
}

func ReadManifest(ctx context.Context, target transport.Transport, n app.Names, id string) (Manifest, error) {
	if !IsID(id) {
		return Manifest{}, manifestInvalid(id, "identifier is invalid")
	}
	path := ManifestPath(n, id)
	command := `if [ ! -e ` + q(path) + ` ]; then exit 3; fi; ` +
		`if [ ! -f ` + q(path) + ` ]; then exit 4; fi; ` +
		`mode=$(stat -c '%a' ` + q(path) + ` 2>/dev/null || stat -f '%Lp' ` + q(path) + ` 2>/dev/null) || exit 5; ` +
		`printf 'mode=%s\n' "$mode"; cat ` + q(path)
	result, err := target.Run(ctx, command)
	if err != nil {
		return Manifest{}, err
	}
	switch result.ExitCode {
	case 0:
	case 3:
		return Manifest{}, &ManifestError{Code: "manifest_missing", ReleaseID: id, Detail: "release has no manifest"}
	case 4:
		return Manifest{}, &ManifestError{Code: "manifest_invalid", ReleaseID: id, Detail: "manifest path is not a regular file"}
	default:
		return Manifest{}, &ManifestError{Code: "manifest_read_failed", ReleaseID: id, Detail: strings.TrimSpace(result.Stderr)}
	}
	mode, body, found := strings.Cut(result.Stdout, "\n")
	if !found || mode != "mode=600" {
		return Manifest{}, &ManifestError{Code: "manifest_mode_unsafe", ReleaseID: id, Detail: "manifest must have mode 0600"}
	}
	manifest, err := DecodeManifest([]byte(body))
	if err != nil {
		if typed, ok := err.(*ManifestError); ok && typed.ReleaseID == "" {
			typed.ReleaseID = id
		}
		return Manifest{}, err
	}
	if manifest.ID != id {
		return Manifest{}, manifestInvalid(id, fmt.Sprintf("manifest identifies release %q", manifest.ID))
	}
	return manifest, nil
}
