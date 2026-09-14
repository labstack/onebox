package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// The image provides public certificate authorities. When the host has a
// bundle, stage it so private endpoint roots work as they do for Docker.
func TestStagingTheRuntimeCopiesTheHostTrustStoreInBesideTheBinary(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)

	if err := engine.stageTrustStore(context.Background(), "database"); err != nil {
		t.Fatalf("staging the trust store: %v", err)
	}

	staged := engine.names().BackupTrustStoreFile("database")
	probe := strings.Join(fake.Commands, "\n")
	if !strings.Contains(probe, "/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("the Debian and Ubuntu bundle was never looked for:\n%s", probe)
	}
	if !strings.Contains(probe, staged) {
		t.Errorf("nothing was copied to %s:\n%s", staged, probe)
	}
	// Written to a temporary name and renamed, like every other generated
	// file. The mount is of the directory, so the new inode is still reachable.
	if !strings.Contains(probe, staged+".tmp") {
		t.Errorf("the bundle was written in place rather than renamed over:\n%s", probe)
	}
}

func TestStagingTheRuntimeCreatesTheAdapterDirectory(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)

	if err := engine.StageBackupRuntime(context.Background(), "database", []byte("#!/bin/sh\n")); err != nil {
		t.Fatalf("staging the backup adapter: %v", err)
	}
	commands := strings.Join(fake.Commands, "\n")
	want := engine.names().BackupAdapterDir("database")
	if !strings.Contains(commands, "mkdir -p") || !strings.Contains(commands, want) {
		t.Errorf("adapter directory %s was not created:\n%s", want, commands)
	}
}
