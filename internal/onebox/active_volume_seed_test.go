package onebox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

func activeVolumeSeedEngine(t *testing.T, fake *transport.Fake) (*engine.Engine, int) {
	t.Helper()
	resolved := &app.Resolved{
		Spec: &app.Spec{Name: "example", BasePath: "/var/lib/ob", Services: map[string]app.Service{"database": {Driver: "postgres", Version: 17}}},
		Env:  "production",
	}
	execution := engine.New(resolved, nil, fake, engine.Options{Out: io.Discard, LockTTL: time.Minute})
	appEpoch, err := execution.AcquireLock(context.Background(), "seed-1", false)
	if err != nil {
		t.Fatalf("acquire app lock: %v", err)
	}
	if err := execution.WriteFence(context.Background(), "seed-1", appEpoch); err != nil {
		t.Fatalf("write app fence: %v", err)
	}
	serviceEpoch, err := execution.AcquireProtectionLock(context.Background(), "database", "seed-1", 0)
	if err != nil {
		t.Fatalf("acquire protection lock: %v", err)
	}
	return execution, serviceEpoch
}

func TestSeedActiveVolumeFreshMigrationRecordsOwnedStableVolumeOnly(t *testing.T) {
	fake := &transport.Fake{}
	execution, epoch := activeVolumeSeedEngine(t, fake)
	fake.Dynamic = func(command string) (transport.Result, bool) {
		switch {
		case strings.HasPrefix(command, "if [ -f ") && strings.Contains(command, "active-volume.json"):
			return transport.Result{ExitCode: 3}, true
		case strings.HasPrefix(command, "docker volume inspect"):
			return transport.Result{Stdout: "ob_example_database\n"}, true
		}
		return transport.Result{}, false
	}

	record, seeded, err := SeedActiveVolume(context.Background(), execution, "database", "data", "seed-1", epoch)
	if err != nil {
		t.Fatalf("seed active volume: %v", err)
	}
	if !seeded || record.SelectedVolume != "ob_example_database_data" || record.Epoch != epoch {
		t.Fatalf("seeded active volume = %#v, %v", record, seeded)
	}
	for _, command := range fake.Commands {
		if strings.Contains(command, "docker volume create") || strings.Contains(command, "docker volume rm") || strings.Contains(command, "docker volume cp") {
			t.Fatalf("active-volume seed mutated Docker volumes: %s", command)
		}
	}
}

func TestSeedActiveVolumeExistingStateIsIdempotent(t *testing.T) {
	fake := &transport.Fake{}
	execution, _ := activeVolumeSeedEngine(t, fake)
	existing, err := NewActiveVolumeRecord("example", "production", "database", "data", "ob_example_database_restore_7", "restore-7", 7,
		&ActiveVolumeSelection{DockerVolume: "ob_example_database_data", OperationID: "seed-1", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeActiveVolumeRecord(existing)
	if err != nil {
		t.Fatal(err)
	}
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.HasPrefix(command, "if [ -f ") && strings.Contains(command, "active-volume.json") {
			return transport.Result{Stdout: string(encoded)}, true
		}
		return transport.Result{}, false
	}

	record, seeded, err := SeedActiveVolume(context.Background(), execution, "database", "data", "seed-8", 8)
	if err != nil {
		t.Fatal(err)
	}
	if seeded || record.RecordDigest != existing.RecordDigest {
		t.Fatalf("existing state changed: %#v, seeded=%v", record, seeded)
	}
	for _, command := range fake.Commands {
		if strings.HasPrefix(command, "docker volume inspect") {
			t.Fatal("existing active-volume state triggered volume adoption")
		}
	}
}

func TestSeedActiveVolumeRefusesMissingStableVolume(t *testing.T) {
	fake := &transport.Fake{}
	execution, epoch := activeVolumeSeedEngine(t, fake)
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.HasPrefix(command, "if [ -f ") && strings.Contains(command, "active-volume.json") {
			return transport.Result{ExitCode: 3}, true
		}
		if strings.HasPrefix(command, "docker volume inspect") {
			return transport.Result{ExitCode: 1}, true
		}
		return transport.Result{}, false
	}
	if _, _, err := SeedActiveVolume(context.Background(), execution, "database", "data", "seed-1", epoch); err == nil || !strings.Contains(err.Error(), "no volume was created") {
		t.Fatalf("missing stable volume error = %v", err)
	}
}

func TestSeedActiveVolumeRefusesForeignCollision(t *testing.T) {
	fake := &transport.Fake{}
	execution, epoch := activeVolumeSeedEngine(t, fake)
	fake.Dynamic = func(command string) (transport.Result, bool) {
		if strings.HasPrefix(command, "if [ -f ") && strings.Contains(command, "active-volume.json") {
			return transport.Result{ExitCode: 3}, true
		}
		if strings.HasPrefix(command, "docker volume inspect") {
			return transport.Result{Stdout: "foreign_project\n"}, true
		}
		return transport.Result{}, false
	}
	if _, _, err := SeedActiveVolume(context.Background(), execution, "database", "data", "seed-1", epoch); err == nil || !strings.Contains(err.Error(), "refuses volume") {
		t.Fatalf("foreign collision error = %v", err)
	}
}
