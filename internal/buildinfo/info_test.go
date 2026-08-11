package buildinfo

import (
	"os"
	"runtime/debug"
	"testing"
)

func TestReadDerivesEmbeddedBuildInfo(t *testing.T) {
	got := read(&debug.BuildInfo{
		GoVersion: "go-test",
		Main:      debug.Module{Version: "v2026.7.1"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.time", Value: "2026-07-13T12:34:56Z"},
		},
	}, true, "", "2026-07-13T12:35:00Z")

	if got.Version != "v2026.7.1" || got.VCSRevision != "abc123" || !got.Dirty {
		t.Fatalf("unexpected version provenance: %+v", got)
	}
	if got.VCSTime != "2026-07-13T12:34:56Z" || got.BuildTime != "2026-07-13T12:35:00Z" {
		t.Fatalf("unexpected time provenance: %+v", got)
	}
	if got.GoVersion != "go-test" {
		t.Fatalf("go version = %q, want go-test", got.GoVersion)
	}
}

func TestReadPrefersLinkedRelease(t *testing.T) {
	got := read(&debug.BuildInfo{Main: debug.Module{Version: "v2026.7.1"}}, true, "v2026.8.0", "")
	if got.Version != "v2026.8.0" {
		t.Fatalf("version = %q, want linked release", got.Version)
	}
}

func TestReadUsesDevelopmentVersionForCheckoutBuild(t *testing.T) {
	got := read(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "", "")
	if got.Version != developmentVersion {
		t.Fatalf("version = %q, want %q", got.Version, developmentVersion)
	}
}

func TestReadFileInspectsGoExecutableWithoutRunningIt(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("inspect test executable: %v", err)
	}
	if got.GoVersion == "" {
		t.Fatal("inspected Go version is empty")
	}
}
