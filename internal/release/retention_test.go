package release

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func retentionManifest(t *testing.T, id string, kind ManifestKind, state State, predecessor string, at time.Time) Manifest {
	t.Helper()
	manifest, err := NewManifest(id, kind, at)
	if err != nil {
		t.Fatal(err)
	}
	switch string(kind) + "/" + string(state) {
	case string(KindBootstrap) + "/" + string(StateVerified):
		err = manifest.Transition(StateVerified, at, "")
	case string(KindApplication) + "/" + string(StateFailed):
		err = manifest.Transition(StateFailed, at, "")
	case string(KindApplication) + "/" + string(StateVerified):
		err = manifest.Transition(StateVerified, at, "")
	case string(KindApplication) + "/" + string(StateServing):
		err = manifest.Transition(StateVerified, at, "")
		if err == nil {
			err = manifest.Transition(StateServing, at, predecessor)
		}
	case string(KindApplication) + "/" + string(StateSuperseded):
		err = manifest.Transition(StateVerified, at, "")
		if err == nil {
			err = manifest.Transition(StateServing, at, predecessor)
		}
		if err == nil {
			err = manifest.Transition(StateSuperseded, at, "")
		}
	default:
		t.Fatalf("unsupported test manifest %s/%s", kind, state)
	}
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeRetentionManifest(t *testing.T, target *transport.Fake, names app.Names, manifest Manifest) {
	t.Helper()
	if err := WriteManifest(context.Background(), target, names, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionProtectsCurrentChainAndCheckpointReferences(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := []string{
		"20260101-000000-aaa",
		"20260102-000000-bbb",
		"20260103-000000-ccc",
		"20260104-000000-ddd",
	}
	target := &transport.Fake{}
	writeRetentionManifest(t, target, names, retentionManifest(t, ids[0], KindApplication, StateSuperseded, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, ids[1], KindApplication, StateSuperseded, ids[0], old))
	writeRetentionManifest(t, target, names, retentionManifest(t, ids[2], KindApplication, StateServing, ids[1], old))
	writeRetentionManifest(t, target, names, retentionManifest(t, ids[3], KindApplication, StateFailed, "", old))
	checkpoint, err := NewActivationCheckpoint(ids[3], ids[2], ActivationPrepared, old)
	if err != nil {
		t.Fatal(err)
	}
	command, input, err := ActivationCheckpointWrite(names, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.RunInput(context.Background(), command, input); err != nil {
		t.Fatal(err)
	}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: strings.Join(ids, "\n") + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + ids[2] + "\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}
	policy := DefaultRetentionPolicy(2, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
	decision, err := RetentionCandidates(context.Background(), target, names, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decision.Victims, []string{ids[0]}) {
		t.Fatalf("victims = %v", decision.Victims)
	}
	for _, protected := range []string{ids[1], ids[2], ids[3]} {
		if !slices.Contains(decision.Preserve, protected) {
			t.Errorf("checkpoint/chain release %s was not preserved: %+v", protected, decision)
		}
	}
}

func TestRetentionProtectsReleaseLeasedByScheduledJob(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leasedID := "20260101-000000-leased"
	currentID := "20260102-000000-current"
	target := &transport.Fake{}
	writeRetentionManifest(t, target, names, retentionManifest(t, leasedID, KindApplication, StateSuperseded, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, currentID, KindApplication, StateServing, "", old))
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: leasedID + "\n" + currentID + "\n"}, true
		case strings.Contains(command, ".ob-schedule.lease"):
			return transport.Result{Stdout: leasedID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + currentID + "\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	decision, err := RetentionCandidates(context.Background(), target, names,
		DefaultRetentionPolicy(1, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(decision.Victims, leasedID) || !slices.Contains(decision.Preserve, leasedID) {
		t.Fatalf("leased release was not protected: %+v", decision)
	}
}

func TestRetentionUsesSeparateAgeAndEvidenceRules(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-24 * time.Hour)
	currentID := "20260808-000000-current"
	bootstrapID := "20260101-000000-bootstrap"
	failedOldID := "20260102-000000-failed"
	failedRecentID := "20260808-000000-failed"
	unknownEvidenceID := "20260103-000000-evidence"
	unknownOldID := "20260104-000000-unknown"
	uploadOld := "20260105-000000-upload.partial"
	uploadRecent := "20260808-120000-upload.partial"
	entries := []string{currentID, bootstrapID, failedOldID, failedRecentID, unknownEvidenceID, unknownOldID, uploadOld, uploadRecent}
	target := &transport.Fake{}
	writeRetentionManifest(t, target, names, retentionManifest(t, currentID, KindApplication, StateServing, "", recent))
	writeRetentionManifest(t, target, names, retentionManifest(t, bootstrapID, KindBootstrap, StateVerified, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, failedOldID, KindApplication, StateFailed, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, failedRecentID, KindApplication, StateFailed, "", recent))
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: strings.Join(entries, "\n") + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + currentID + "\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}
	policy := DefaultRetentionPolicy(2, now)
	policy.EvidenceIDs = map[string]bool{unknownEvidenceID: true}
	decision, err := RetentionCandidates(context.Background(), target, names, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, victim := range []string{bootstrapID, failedOldID, unknownOldID, uploadOld} {
		if !slices.Contains(decision.Victims, victim) {
			t.Errorf("expired %s not collected: %+v", victim, decision)
		}
	}
	for _, preserved := range []string{currentID, failedRecentID, unknownEvidenceID, uploadRecent} {
		if !slices.Contains(decision.Preserve, preserved) {
			t.Errorf("protected/recent %s not preserved: %+v", preserved, decision)
		}
	}
}

// "No manifest" and "the manifest could not be read" are different evidence.
// Age can retire the first; deleting on the second destroys the release whose
// own record is what an operator would need to diagnose it — and the -L guard
// that distinguishes the two is worth nothing if retention collapses them.
func TestRetentionKeepsReleasesWhoseManifestIsUnreadable(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	danglingID := "20260104-000000-dangling"  // manifest is a broken symlink
	missingID := "20260104-000000-nomanifest" // manifest genuinely absent
	entries := []string{danglingID, missingID}

	target := &transport.Fake{}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: strings.Join(entries, "\n") + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{}, true // no current release
		case strings.Contains(command, ManifestPath(names, danglingID)):
			return transport.Result{ExitCode: 4}, true // present, not a regular file
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, now))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(decision.Victims, danglingID) {
		t.Errorf("release with an unreadable manifest was collected for deletion: %+v", decision)
	}
	if !slices.Contains(decision.Preserve, danglingID) || !slices.Contains(decision.Reported, danglingID) {
		t.Errorf("release with an unreadable manifest was not preserved and reported: %+v", decision)
	}
	// The distinction has to cut both ways, or it is just a blanket refusal
	// to ever delete.
	if !slices.Contains(decision.Victims, missingID) {
		t.Errorf("expired release with no manifest was not collected: %+v", decision)
	}
}

// A command that fails outright is the least evidence of all: nothing was read,
// so a transient transport error must not hand a release to rm -rf.
func TestRetentionKeepsReleasesWhoseManifestCommandFailed(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	unreachableID := "20260104-000000-unreachable"

	target := &transport.Fake{}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: unreachableID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}
	// A bare, untyped error from the command itself — not a *ManifestError.
	target.Err = func(command string) error {
		if strings.Contains(command, ManifestPath(names, unreachableID)) {
			return errors.New("connection reset")
		}
		return nil
	}

	decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, now))
	if err != nil {
		// Refusing the whole run is also acceptable; deleting is not.
		return
	}
	if slices.Contains(decision.Victims, unreachableID) {
		t.Errorf("release whose manifest command failed was collected for deletion: %+v", decision)
	}
}

// A schema this binary does not support is "cannot read", not "is wrong".
// After a downgrade past a schema bump every release the newer binary wrote
// reads this way, and retiring them by age deletes exactly the releases an
// operator would roll forward to.
func TestRetentionKeepsReleasesWrittenByANewerSchema(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	futureID := "20260104-000000-future"

	target := &transport.Fake{}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: futureID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{}, true
		case strings.Contains(command, ManifestPath(names, futureID)):
			return transport.Result{Stdout: "mode=600\n{\"schema_version\":\"onebox.run/v99\"}\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, now))
	if err != nil {
		return // refusing the run is also acceptable; deleting is not
	}
	if slices.Contains(decision.Victims, futureID) {
		t.Errorf("release written by a newer schema was collected for deletion: %+v", decision)
	}
}

// A manifest that was read and found wrong is a verdict, not a failed read.
// Preserving on it strands the release forever: the condition never heals, so
// every GC run repeats the same unactionable report and nothing is reclaimed.
func TestRetentionStillRetiresReleasesWithABadManifest(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	corruptID := "20260104-000000-corrupt"

	target := &transport.Fake{}
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: corruptID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{}, true
		case strings.Contains(command, ManifestPath(names, corruptID)):
			// Read fine, mode fine, body is not a manifest.
			return transport.Result{Stdout: "mode=600\nnot-json\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, now))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(decision.Victims, corruptID) {
		t.Errorf("expired release with a corrupt manifest was not collected: %+v", decision)
	}
}

func TestRetentionRefusesUnusableCheckpointEvidence(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{}, true
		case strings.Contains(command, "/activation.json") && strings.Contains(command, "mode=%s"):
			return transport.Result{Stdout: "mode=600\nnot-json"}, true
		}
		return transport.Result{}, false
	}}
	_, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, time.Now()))
	if err == nil || !strings.Contains(err.Error(), "checkpoint evidence is unusable") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetentionRefusesUnusablePredecessorEvidence(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	currentID := "20260808-000000-current"
	target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: currentID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + currentID + "\n"}, true
		case strings.Contains(command, currentID+"/manifest.json"):
			return transport.Result{ExitCode: 5, Stderr: "permission denied"}, true
		}
		return transport.Result{}, false
	}}
	decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(2, time.Now()))
	if err == nil || !strings.Contains(err.Error(), "predecessor chain evidence is unusable") {
		t.Fatalf("error = %v", err)
	}
	if len(decision.Victims) != 0 {
		t.Fatalf("retention selected victims after unreadable predecessor evidence: %+v", decision)
	}
}

func TestRetentionRejectsZeroAgeWindows(t *testing.T) {
	policy := DefaultRetentionPolicy(2, time.Now())
	policy.FailedAfter = 0
	_, err := RetentionCandidates(context.Background(), &transport.Fake{}, app.Names{App: "sample", BasePath: app.DefaultBasePath}, policy)
	if err == nil || !strings.Contains(err.Error(), "policy is invalid") {
		t.Fatalf("error = %v", err)
	}
}

func TestRetentionProtectsReleaseMountedByLiveContainer(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mountedID := "20260101-000000-mounted"
	currentID := "20260102-000000-current"
	target := &transport.Fake{}
	writeRetentionManifest(t, target, names, retentionManifest(t, mountedID, KindApplication, StateSuperseded, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, currentID, KindApplication, StateServing, "", old))
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: mountedID + "\n" + currentID + "\n"}, true
		case strings.Contains(command, "docker ps"):
			return transport.Result{Stdout: mountedID + "\n" + currentID + "\n"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + currentID + "\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	decision, err := RetentionCandidates(context.Background(), target, names,
		DefaultRetentionPolicy(1, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(decision.Victims, mountedID) {
		t.Fatalf("a release a running container still mounts was selected for deletion: %+v", decision)
	}
	if !slices.Contains(decision.Preserve, mountedID) {
		t.Fatalf("mounted release was not preserved: %+v", decision)
	}
}

func TestRetentionRefusesWhenLiveContainerEvidenceIsUnusable(t *testing.T) {
	names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleID := "20260101-000000-stale"
	currentID := "20260102-000000-current"
	target := &transport.Fake{}
	writeRetentionManifest(t, target, names, retentionManifest(t, staleID, KindApplication, StateSuperseded, "", old))
	writeRetentionManifest(t, target, names, retentionManifest(t, currentID, KindApplication, StateServing, "", old))
	base := target.Dynamic
	target.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.Contains(command, "ls -1A"):
			return transport.Result{Stdout: staleID + "\n" + currentID + "\n"}, true
		case strings.Contains(command, "docker ps"):
			return transport.Result{ExitCode: 1, Stderr: "cannot connect to the docker daemon"}, true
		case strings.Contains(command, "readlink"):
			return transport.Result{Stdout: "releases/" + currentID + "\n"}, true
		}
		if base != nil {
			return base(command)
		}
		return transport.Result{}, false
	}

	_, err := RetentionCandidates(context.Background(), target, names,
		DefaultRetentionPolicy(1, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)))
	var evidence *RetentionEvidenceError
	if !errors.As(err, &evidence) {
		t.Fatalf("error = %v, want a retention evidence refusal", err)
	}
}
