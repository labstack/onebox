package release

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

var (
	manifestID   = "20260809-120000-abc1234"
	predecessor  = "20260808-120000-def5678"
	manifestTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
)

func servingManifest(t *testing.T) Manifest {
	t.Helper()
	manifest, err := NewManifest(manifestID, KindApplication, manifestTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Transition(StateVerified, manifestTime.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Transition(StateServing, manifestTime.Add(2*time.Second), predecessor); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestManifestStateMachineSeparatesServingStateFromOperationOutcome(t *testing.T) {
	manifest := servingManifest(t)
	if err := manifest.RecordOperationOutcome(OutcomeFailed, manifestTime.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if manifest.State != StateServing || manifest.OperationOutcome != OutcomeFailed || manifest.Predecessor != predecessor {
		t.Fatalf("post-activation failure changed lifecycle truth: %#v", manifest)
	}
	if err := manifest.Transition(StateSuperseded, manifestTime.Add(4*time.Second), predecessor); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Transition(StateServing, manifestTime.Add(5*time.Second), manifestID); err == nil {
		t.Fatal("rollback activation accepted a self predecessor")
	}
	left := "20260810-120000-aaa9999"
	if err := manifest.Transition(StateServing, manifestTime.Add(5*time.Second), left); err != nil {
		t.Fatal(err)
	}
	if manifest.Predecessor != left || manifest.OperationOutcome != OutcomeSucceeded {
		t.Fatalf("rollback activation = %#v", manifest)
	}
}

func TestManifestRejectsNonMonotonicAndKindInvalidTransitions(t *testing.T) {
	manifest, err := NewManifest(manifestID, KindApplication, manifestTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Transition(StateServing, manifestTime.Add(time.Second), ""); err == nil {
		t.Fatal("staged release skipped verification")
	}
	if err := manifest.Transition(StateVerified, manifestTime.Add(-time.Second), ""); err == nil {
		t.Fatal("transition timestamp moved backwards")
	}
	bootstrap, err := NewManifest(manifestID, KindBootstrap, manifestTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Transition(StateVerified, manifestTime.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if bootstrap.OperationOutcome != OutcomeSucceeded {
		t.Fatalf("bootstrap outcome = %q", bootstrap.OperationOutcome)
	}
	if err := bootstrap.Transition(StateServing, manifestTime.Add(2*time.Second), ""); err == nil {
		t.Fatal("bootstrap became a serving application release")
	}
}

func TestDecodeManifestIsClosedAndValidatesEvidence(t *testing.T) {
	valid, err := EncodeManifest(servingManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(valid, &document); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(map[string]any)
		code string
	}{
		{"unknown field", func(doc map[string]any) { doc["mystery"] = true }, "manifest_invalid"},
		{"unknown schema", func(doc map[string]any) { doc["schema_version"] = "onebox.run/release-manifest/v2" }, "manifest_schema_unknown"},
		{"self predecessor", func(doc map[string]any) { doc["predecessor"] = manifestID }, "manifest_invalid"},
		{"state mismatch", func(doc map[string]any) { doc["state"] = StateVerified }, "manifest_invalid"},
		{"bad outcome", func(doc map[string]any) { doc["operation_outcome"] = OutcomePending }, "manifest_invalid"},
		{"bad transition", func(doc map[string]any) {
			transitions := doc["transitions"].([]any)
			transitions[1].(map[string]any)["state"] = StateSuperseded
		}, "manifest_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyBody, _ := json.Marshal(document)
			var copyDocument map[string]any
			_ = json.Unmarshal(copyBody, &copyDocument)
			test.edit(copyDocument)
			body, _ := json.Marshal(copyDocument)
			_, err := DecodeManifest(body)
			var manifestErr *ManifestError
			if !errors.As(err, &manifestErr) || manifestErr.Code() != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestManifestWriterIsAtomicAndModeProtected(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(names.ReleaseDir(manifestID), 0o755); err != nil {
		t.Fatal(err)
	}
	target := transport.NewLocal()
	manifest := servingManifest(t)
	if err := WriteManifest(context.Background(), target, names, manifest); err != nil {
		t.Fatal(err)
	}
	path := ManifestPath(names, manifestID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o", info.Mode().Perm())
	}
	read, err := ReadManifest(context.Background(), target, names, manifestID)
	if err != nil || read.State != StateServing || read.Predecessor != predecessor {
		t.Fatalf("read manifest = %#v, %v", read, err)
	}
	matches, err := filepath.Glob(path + ".tmp.*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary manifests remain: %v (%v)", matches, err)
	}
}

func TestConcurrentManifestWritesNeverExposePartialJSON(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(names.ReleaseDir(manifestID), 0o755); err != nil {
		t.Fatal(err)
	}
	target := transport.NewLocal()
	manifests := []Manifest{servingManifest(t), servingManifest(t)}
	if err := manifests[1].RecordOperationOutcome(OutcomeFailed, manifestTime.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wait.Add(1)
		go func(manifest Manifest) {
			defer wait.Done()
			errorsSeen <- WriteManifest(context.Background(), target, names, manifest)
		}(manifests[i%len(manifests)])
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(ManifestPath(names, manifestID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(body); err != nil {
		t.Fatalf("concurrent atomic writes exposed invalid JSON: %v\n%s", err, body)
	}
}

func TestReadManifestReportsMissingUnsafeAndMismatchedEvidence(t *testing.T) {
	base := t.TempDir()
	names := app.Names{App: "sample", BasePath: base}
	if err := os.MkdirAll(names.ReleaseDir(manifestID), 0o755); err != nil {
		t.Fatal(err)
	}
	target := transport.NewLocal()
	assertCode := func(want string, err error) {
		t.Helper()
		var manifestErr *ManifestError
		if !errors.As(err, &manifestErr) || manifestErr.Code() != want {
			t.Fatalf("error = %v, want %s", err, want)
		}
	}
	_, err := ReadManifest(context.Background(), target, names, manifestID)
	assertCode("manifest_missing", err)

	body, _ := EncodeManifest(servingManifest(t))
	path := ManifestPath(names, manifestID)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ReadManifest(context.Background(), target, names, manifestID)
	assertCode("manifest_mode_unsafe", err)

	other := strings.Replace(string(body), manifestID, "20260809-120000-fffffff", 1)
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadManifest(context.Background(), target, names, manifestID)
	assertCode("manifest_invalid", err)
}
