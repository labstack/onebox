package release

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const (
	DefaultFailedReleaseGC    = 7 * 24 * time.Hour
	DefaultBootstrapReleaseGC = 7 * 24 * time.Hour
	DefaultUploadGC           = 24 * time.Hour
	DefaultUnknownReleaseGC   = 30 * 24 * time.Hour
)

// RetentionPolicy is intentionally independent from rollback eligibility.
// RetainApplications bounds the healthy predecessor chain; explicit ages and
// durable evidence govern everything that was never a rollback candidate.
type RetentionPolicy struct {
	RetainApplications int
	Now                time.Time
	FailedAfter        time.Duration
	BootstrapAfter     time.Duration
	UploadAfter        time.Duration
	UnknownAfter       time.Duration
	EvidenceIDs        map[string]bool
}

type RetentionDecision struct {
	Victims  []string
	Preserve []string
	Reported []string
}

// RetentionEvidenceError reports that the protected set could not be
// established from durable evidence, so no deletion candidate was selected.
//
// It is typed because refusing to delete and failing the operation in flight
// are different decisions, and only the caller can make the second one. Nothing
// is lost by cleaning up later; a healthy activated release stranded without a
// terminal state because cleanup declined to act is a real loss.
type RetentionEvidenceError struct{ Err error }

func (err *RetentionEvidenceError) Error() string {
	return fmt.Sprintf("retention refused: %v", err.Err)
}

func (err *RetentionEvidenceError) Unwrap() error { return err.Err }

// refuseRetention classifies a failed evidence read. A cancelled or expired
// context is not evidence about the release store — it says the operator or the
// deadline stopped this run — so it stays a hard failure rather than being
// downgraded to a reported skip that would let the caller issue more commands.
func refuseRetention(cause, refusal error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return &RetentionEvidenceError{Err: refusal}
}

func DefaultRetentionPolicy(retain int, now time.Time) RetentionPolicy {
	if retain < 1 {
		retain = 1
	}
	return RetentionPolicy{
		RetainApplications: retain,
		Now:                now.UTC(),
		FailedAfter:        DefaultFailedReleaseGC,
		BootstrapAfter:     DefaultBootstrapReleaseGC,
		UploadAfter:        DefaultUploadGC,
		UnknownAfter:       DefaultUnknownReleaseGC,
	}
}

func RetentionCandidates(ctx context.Context, target transport.Transport, names app.Names, policy RetentionPolicy) (RetentionDecision, error) {
	if policy.Now.IsZero() || policy.RetainApplications < 1 || policy.FailedAfter <= 0 || policy.BootstrapAfter <= 0 || policy.UploadAfter <= 0 || policy.UnknownAfter <= 0 {
		return RetentionDecision{}, fmt.Errorf("retention policy is invalid")
	}
	ids, skipped, err := list(ctx, target, names)
	if err != nil {
		return RetentionDecision{}, err
	}
	decision := RetentionDecision{}
	protected := map[string]bool{}
	for id := range policy.EvidenceIDs {
		protected[id] = true
	}

	current, err := Current(ctx, target, names)
	if err != nil {
		return RetentionDecision{}, err
	}
	if current != "" {
		protected[current] = true
		if err := protectPredecessorChain(ctx, target, names, current, policy.RetainApplications, protected); err != nil {
			return RetentionDecision{}, refuseRetention(err, fmt.Errorf("predecessor chain evidence is unusable: %w", err))
		}
	}
	checkpoint, checkpointErr := ReadActivationCheckpoint(ctx, target, names)
	if checkpointErr == nil {
		protected[checkpoint.ReleaseID] = true
		if checkpoint.Predecessor != "" {
			protected[checkpoint.Predecessor] = true
		}
	} else if !errors.Is(checkpointErr, ErrActivationCheckpointMissing) {
		return RetentionDecision{}, refuseRetention(checkpointErr, fmt.Errorf("activation checkpoint evidence is unusable: %w", checkpointErr))
	}
	secretCheckpoint, secretCheckpointErr := ReadSecretCheckpoint(ctx, target, names)
	if secretCheckpointErr == nil {
		protected[secretCheckpoint.ReleaseID] = true
	} else if !errors.Is(secretCheckpointErr, ErrSecretCheckpointMissing) {
		return RetentionDecision{}, refuseRetention(secretCheckpointErr, fmt.Errorf("secret checkpoint evidence is unusable: %w", secretCheckpointErr))
	}

	for _, id := range ids {
		if protected[id] {
			decision.Preserve = append(decision.Preserve, id)
			continue
		}
		manifest, manifestErr := ReadManifest(ctx, target, names, id)
		if manifestErr != nil {
			if policy.EvidenceIDs[id] || !expiredReleaseID(id, policy.Now, policy.UnknownAfter) {
				decision.Preserve = append(decision.Preserve, id)
				decision.Reported = append(decision.Reported, id)
				continue
			}
			decision.Victims = append(decision.Victims, id)
			continue
		}
		age := policy.Now.Sub(manifestLastTransition(manifest))
		switch manifest.Kind {
		case KindBootstrap:
			if age >= policy.BootstrapAfter {
				decision.Victims = append(decision.Victims, id)
			} else {
				decision.Preserve = append(decision.Preserve, id)
			}
		case KindApplication:
			switch manifest.State {
			case StateSuperseded:
				decision.Victims = append(decision.Victims, id)
			case StateStaged, StateFailed, StateAborted:
				if age >= policy.FailedAfter {
					decision.Victims = append(decision.Victims, id)
				} else {
					decision.Preserve = append(decision.Preserve, id)
				}
			default:
				// An unprotected serving or verified release is state disagreement,
				// not garbage. Preserve it and make the anomaly visible.
				decision.Preserve = append(decision.Preserve, id)
				decision.Reported = append(decision.Reported, id)
			}
		}
	}

	for _, entry := range skipped {
		base := strings.TrimSuffix(entry, ".partial")
		if strings.HasSuffix(entry, ".partial") && !policy.EvidenceIDs[entry] && expiredReleaseID(base, policy.Now, policy.UploadAfter) {
			decision.Victims = append(decision.Victims, entry)
			continue
		}
		decision.Preserve = append(decision.Preserve, entry)
		decision.Reported = append(decision.Reported, entry)
	}
	return decision, nil
}

func protectPredecessorChain(ctx context.Context, target transport.Transport, names app.Names, current string, retain int, protected map[string]bool) error {
	id := current
	for depth := 0; id != "" && depth < retain; depth++ {
		protected[id] = true
		manifest, err := ReadManifest(ctx, target, names, id)
		if err != nil {
			return err
		}
		id = manifest.Predecessor
	}
	return nil
}

func manifestLastTransition(manifest Manifest) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, manifest.Transitions[len(manifest.Transitions)-1].At)
	return at
}

func expiredReleaseID(id string, now time.Time, after time.Duration) bool {
	if len(id) < len("20060102-150405") {
		return false
	}
	created, err := time.Parse("20060102-150405", id[:len("20060102-150405")])
	return err == nil && !now.Before(created.Add(after))
}
