package onebox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
)

type fakeScheduledArtifactTarget struct {
	artifacts map[string]ScheduledInstallArtifact
	installs  int
	removes   []string
	inspect   func(ScheduledInstallArtifact) ScheduledInstalledMetadata
}

func (target *fakeScheduledArtifactTarget) InstallAtomic(_ context.Context, artifact ScheduledInstallArtifact) error {
	if target.artifacts == nil {
		target.artifacts = map[string]ScheduledInstallArtifact{}
	}
	copy := artifact
	copy.Content = append([]byte(nil), artifact.Content...)
	target.artifacts[artifact.Path] = copy
	target.installs++
	return nil
}

func (target *fakeScheduledArtifactTarget) Inspect(_ context.Context, path string) (ScheduledInstalledMetadata, error) {
	artifact, ok := target.artifacts[path]
	if !ok {
		return ScheduledInstalledMetadata{}, errors.New("not found")
	}
	if target.inspect != nil {
		return target.inspect(artifact), nil
	}
	return ScheduledInstalledMetadata{Path: path, Mode: artifact.Mode, Owner: artifact.Owner, Group: artifact.Group, Digest: digestScheduledBytes(artifact.Content)}, nil
}

func (target *fakeScheduledArtifactTarget) Remove(_ context.Context, path string) error {
	if _, ok := target.artifacts[path]; !ok {
		return errors.New("not found")
	}
	delete(target.artifacts, path)
	target.removes = append(target.removes, path)
	return nil
}

func scheduledInstallFixture(t *testing.T, runnerBytes []byte) (ScheduledInstallPlan, ScheduledOperationEnvelope, app.Names) {
	t.Helper()
	names := app.Names{App: "example", BasePath: "/var/lib/onebox"}
	runnerDigest := digestScheduledBytes(runnerBytes)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	runner := ScheduledRunnerArtifactReference{
		Path: names.ProtectionRunnerPath(runnerDigest), Digest: runnerDigest,
		SBOMDigest: "sha256:" + strings.Repeat("a", 64), ProvenanceID: "onebox-runner-v1",
	}
	envelope, err := NewScheduledOperationEnvelope(ScheduledEnvelopeInput{
		CLIProtocol:     CurrentScheduledCLIProtocol,
		RunnerProtocols: ProtocolRange{Minimum: CurrentScheduledRunnerProtocol, Maximum: CurrentScheduledRunnerProtocol},
		OperationID:     "backup-20260807", Application: "example", Environment: "production", Service: "database",
		Operation: KindBackupCreate, Runner: runner,
		Timing: ScheduledTimingPolicy{
			ScheduledFor: now.Format(time.RFC3339Nano), NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano),
			ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano), MaxRuntime: "10m", RetryIdentity: "backup-window-20260807",
		},
		Artifacts: []OperationArtifactBinding{
			{Class: "inputs", Path: "/var/lib/onebox/example/protection/inputs.json", Mode: 0o600, Digest: "sha256:" + strings.Repeat("b", 64)},
		},
		State: ScheduledStateBinding{Path: "/var/lib/onebox/example/protection/state.json", Digest: "sha256:" + strings.Repeat("c", 64), Epoch: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := ScheduledPublicationProof{
		ArtifactDigest: runnerDigest, SBOMDigest: runner.SBOMDigest, ProvenanceID: runner.ProvenanceID,
		Publisher: "labstack-onebox", VerificationMethod: "sigstore-bundle",
		EvidenceDigest: "sha256:" + strings.Repeat("d", 64), VerifiedAt: now.Format(time.RFC3339Nano), Verified: true,
	}
	plan, err := NewScheduledInstallPlan(names, runnerBytes, envelope, proof)
	if err != nil {
		t.Fatal(err)
	}
	return plan, envelope, names
}

func TestScheduledInstallEnforcesDigestProvenanceOwnershipAndModes(t *testing.T) {
	plan, _, _ := scheduledInstallFixture(t, []byte("runner-v1"))
	target := &fakeScheduledArtifactTarget{}
	if err := ApplyScheduledInstall(context.Background(), target, plan); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range plan.Artifacts {
		installed := target.artifacts[artifact.Path]
		if installed.Owner != "onebox" || installed.Group != "onebox" || installed.Mode != artifact.Mode || digestScheduledBytes(installed.Content) != artifact.Digest {
			t.Fatalf("installed artifact = %#v", installed)
		}
	}
	// Replay converges to the same two paths and exact bytes.
	if err := ApplyScheduledInstall(context.Background(), target, plan); err != nil {
		t.Fatal(err)
	}
	if len(target.artifacts) != 2 || target.installs != 4 {
		t.Fatalf("replayed install paths=%d calls=%d", len(target.artifacts), target.installs)
	}
}

func TestScheduledInstallRejectsDigestProvenanceAndModeMismatch(t *testing.T) {
	plan, envelope, names := scheduledInstallFixture(t, []byte("runner-v1"))
	badProof := plan.PublicationProof
	badProof.Verified = false
	if _, err := NewScheduledInstallPlan(names, []byte("runner-v1"), envelope, badProof); err == nil {
		t.Fatal("unverified runner provenance was accepted")
	}
	plan.Artifacts[1].Content = []byte("tampered-runner")
	if err := ApplyScheduledInstall(context.Background(), &fakeScheduledArtifactTarget{}, plan); err == nil || !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("runner digest mismatch error = %v", err)
	}
	plan, _, _ = scheduledInstallFixture(t, []byte("runner-v1"))
	target := &fakeScheduledArtifactTarget{inspect: func(artifact ScheduledInstallArtifact) ScheduledInstalledMetadata {
		return ScheduledInstalledMetadata{Path: artifact.Path, Mode: 0o777, Owner: artifact.Owner, Group: artifact.Group, Digest: artifact.Digest}
	}}
	if err := ApplyScheduledInstall(context.Background(), target, plan); err == nil || !strings.Contains(err.Error(), "ownership, mode, or digest") {
		t.Fatalf("mode enforcement error = %v", err)
	}
}

func TestScheduledRunnerUpgradeRetainsOldBinaryUntilAuthorizedUnreferencedRemoval(t *testing.T) {
	oldPlan, _, _ := scheduledInstallFixture(t, []byte("runner-v1"))
	newPlan, _, _ := scheduledInstallFixture(t, []byte("runner-v2"))
	target := &fakeScheduledArtifactTarget{}
	if err := ApplyScheduledInstall(context.Background(), target, oldPlan); err != nil {
		t.Fatal(err)
	}
	if err := ApplyScheduledInstall(context.Background(), target, newPlan); err != nil {
		t.Fatal(err)
	}
	oldRunner := oldPlan.Runner.Path
	newRunner := newPlan.Runner.Path
	if oldRunner == newRunner || target.artifacts[oldRunner].Path == "" || target.artifacts[newRunner].Path == "" {
		t.Fatalf("upgrade runner layout old=%q new=%q artifacts=%#v", oldRunner, newRunner, target.artifacts)
	}
	resources := []ProtectionResource{
		{Identity: oldRunner, Kind: ProtectionResourceRunner, OwnerApplication: "example", OwnerEnvironment: "production", Service: "database"},
		{Identity: newRunner, Kind: ProtectionResourceRunner, OwnerApplication: "example", OwnerEnvironment: "production", Service: "database", Referenced: true},
		{Identity: newPlan.EnvelopePath, Kind: ProtectionResourceEnvelope, OwnerApplication: "example", OwnerEnvironment: "production", Service: "database", Referenced: true},
	}
	inspection, err := InspectProtectionResources("example", "production", resources)
	if err != nil {
		t.Fatal(err)
	}
	removal, err := NewProtectionRemovalPlan(inspection, ProtectionRemovalRequest{
		Mode: KindDestroy, Application: "example", Environment: "production", ProtectionState: "disabled",
		StateDigest: "sha256:" + strings.Repeat("e", 64), PrerequisitesVerifiedAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization := ProtectionRemovalAuthorization{Operation: removal.Mode, PlanDigest: removal.PlanDigest, StateDigest: removal.StateDigest}
	if err := ApplyAuthorizedScheduledRemoval(context.Background(), target, removal, authorization); err != nil {
		t.Fatal(err)
	}
	if _, exists := target.artifacts[oldRunner]; exists {
		t.Fatal("authorized destroy retained the unreferenced old runner")
	}
	if _, exists := target.artifacts[newRunner]; !exists {
		t.Fatal("authorized destroy removed the referenced current runner")
	}
	if _, exists := target.artifacts[newPlan.EnvelopePath]; !exists {
		t.Fatal("authorized destroy removed the referenced envelope")
	}
}
