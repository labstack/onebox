package engine

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// wal-g executes inside the driver's image. `postgres:18` carries no
// certificate authorities, so unless the host's bundle travels with the binary
// every upload to the HTTPS endpoint an s3-compatible target must declare
// fails with "certificate signed by unknown authority" — after the base backup
// has been written and archiving is already on.
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

// A target with no bundle anywhere is refused while the service is still
// exactly as it was. Staging nothing and letting the wrapper fall back to the
// image's empty store reproduces the original failure, only a quarter of an
// hour later and with the database already archiving.
func TestATargetWithNoTrustStoreIsRefusedBeforeArchivingIsTurnedOn(t *testing.T) {
	fake := &transport.Fake{Script: []transport.Rule{
		{Match: regexp.MustCompile("ca-certificates|ca-bundle|cert.pem"), Result: transport.Result{ExitCode: 1}},
	}}
	engine := backupLockTestEngine(fake)

	err := engine.stageTrustStore(context.Background(), "database")
	if err == nil {
		t.Fatal("a target with no certificate authorities was accepted")
	}
	for _, want := range []string{"/etc/ssl/certs/ca-certificates.crt", "ca-certificates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not tell the operator what to install (%q): %v", want, err)
		}
	}
}
