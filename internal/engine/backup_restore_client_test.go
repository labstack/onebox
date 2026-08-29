package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestRecoveredClientCredentialAlreadyMatchesWithoutReconciliation(t *testing.T) {
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "PGCONNECT_TIMEOUT=5") {
			return transport.Result{Stdout: "1\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := backupLockTestEngine(fake)

	if err := engine.ensureRecoveredClientCredential(context.Background(), "restore"); err != nil {
		t.Fatal(err)
	}
	if len(fake.Inputs) != 0 {
		t.Fatalf("matching credential changed the recovered role: %#v", fake.Inputs)
	}
	probe := strings.Join(fake.Commands, "\n")
	if !strings.Contains(probe, "hostname -i") || !strings.Contains(probe, `-h "$host"`) || !strings.Contains(probe, "PGPASSWORD") {
		t.Fatalf("managed client probe did not force TCP password authentication:\n%s", probe)
	}
	if strings.Contains(probe, "-h 127.0.0.1") {
		t.Fatalf("managed client probe used PostgreSQL's trusted loopback rule:\n%s", probe)
	}
}

func TestRecoveredClientCredentialIsReconciledAndVerified(t *testing.T) {
	probes := 0
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "PGCONNECT_TIMEOUT=5") {
			probes++
			if probes == 1 {
				return transport.Result{ExitCode: 2, Stderr: "password authentication failed"}, true
			}
			return transport.Result{Stdout: "1\n"}, true
		}
		return transport.Result{}, false
	}}
	engine := backupLockTestEngine(fake)

	if err := engine.ensureRecoveredClientCredential(context.Background(), "restore"); err != nil {
		t.Fatal(err)
	}
	if probes != 2 {
		t.Fatalf("managed client probes = %d, want initial failure and post-reconciliation proof", probes)
	}
	if len(fake.Inputs) != 1 || !strings.Contains(fake.Inputs[0], `ALTER ROLE "onebox"`) {
		t.Fatalf("reconciliation input = %#v", fake.Inputs)
	}
	if !strings.Contains(fake.Inputs[0], `\getenv ob_managed_password POSTGRES_PASSWORD`) {
		t.Fatalf("reconciliation did not read the credential inside psql: %q", fake.Inputs[0])
	}
}

func TestRecoveredClientCredentialFailureStopsAfterVerification(t *testing.T) {
	probes := 0
	fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
		if strings.Contains(command, "PGCONNECT_TIMEOUT=5") {
			probes++
			return transport.Result{ExitCode: 2, Stderr: "password authentication failed"}, true
		}
		return transport.Result{}, false
	}}
	engine := backupLockTestEngine(fake)

	err := engine.ensureRecoveredClientCredential(context.Background(), "restore")
	if err == nil || !strings.Contains(err.Error(), "does not accept the target-managed client credential") {
		t.Fatalf("verification error = %v", err)
	}
	if probes != 2 || len(fake.Inputs) != 1 {
		t.Fatalf("probes/updates = %d/%d, want 2/1", probes, len(fake.Inputs))
	}
}

func TestRecoveryContainerReceivesTargetServiceCredentialByFile(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)

	err := engine.startRecoveryContainer(context.Background(), "restore", "stage", "postgres:18", nil, "postgres", "offsite")
	if err != nil {
		t.Fatal(err)
	}
	command := fake.Commands[0]
	secret := engine.names().ServiceSecretFile("postgres")
	backup := engine.names().BackupCredentialFile("postgres", "offsite")
	secretAt, backupAt := strings.Index(command, secret), strings.Index(command, backup)
	if secretAt < 0 || backupAt < 0 || secretAt >= backupAt {
		t.Fatalf("recovery env files do not contain service then backup credentials:\n%s", command)
	}
}
