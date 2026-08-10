package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// renderedServices is what generation produces for the fixture's declared
// services: one Compose document per service, each its own project.
func renderedServices(t *testing.T) map[string][]byte {
	t.Helper()
	out, err := testConfig().RenderServices("production")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func accFake(mounts string) *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, ".Mounts") {
			return transport.Result{Stdout: mounts + "\n"}, true
		}
		// The live document of the running service, which the diff is against.
		if strings.Contains(cmd, "cat ") && strings.Contains(cmd, "services/postgres.yaml") {
			return transport.Result{Stdout: "services:\n  postgres:\n    image: postgres:17\n"}, true
		}
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R0\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestServiceApplyConvergesUnderRegime(t *testing.T) {
	f := accFake("") // no mounts on the running service → nothing to lose
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep, Environment: "production"})
	if err := e.ServiceApply(context.Background(), "R9-acc", false); err != nil {
		t.Fatalf("apply: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")

	// Its own project, not the application's: a release must not be able to
	// stop it and a rollback must not be able to remove its volume.
	if !strings.Contains(seq, "docker compose -p 'ob_sample_postgres'") {
		t.Fatalf("service did not converge in its own project:\n%s", seq)
	}
	if strings.Contains(seq, "docker compose -p sample -f") && strings.Contains(seq, "postgres") {
		t.Fatalf("service converged inside the application's project:\n%s", seq)
	}
	for _, c := range f.Commands {
		if strings.Contains(c, "ob_sample_postgres' -f") && !strings.Contains(c, "ob-fenced") {
			t.Fatalf("converge not fenced: %s", c)
		}
	}
	if !strings.Contains(seq, `"phase":"service-apply"`) {
		t.Fatalf("not journaled:\n%s", seq)
	}
	if !strings.Contains(out.String(), "postgres:17") {
		t.Fatalf("diff does not show what would run:\n%s", out.String())
	}
}

func TestServiceApplyStopsWhenJournalStartFails(t *testing.T) {
	f := accFake("")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, `"phase":"service-apply","event":"start"`) {
			return transport.Result{ExitCode: 74, Stderr: "journal is read-only"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ServiceApply(context.Background(), "R9-acc", false)
	if err == nil || !strings.Contains(err.Error(), "journal service apply start") {
		t.Fatalf("service apply error = %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "docker compose -p 'ob_sample_postgres'") {
		t.Fatalf("service apply mutated after journal failure:\n%s", strings.Join(f.Commands, "\n"))
	}
}

// The credential is established once and never rotated: regenerating it would
// leave the application holding a password its database no longer accepts.
func TestServiceApplyEstablishesCredentialWithoutTravelling(t *testing.T) {
	f := accFake("")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	rendered := renderedServices(t)
	if err := e.ServiceApply(context.Background(), "R9-acc", false); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "/var/lib/ob/sample/services/postgres.secret.env") {
		t.Fatalf("no credential established:\n%s", seq)
	}
	if !strings.Contains(seq, "if [ -s '/var/lib/ob/sample/services/postgres.secret.env' ]") {
		t.Fatalf("credential is not established conditionally — a re-apply would rotate it:\n%s", seq)
	}
	if !strings.Contains(seq, "POSTGRES_URL") {
		t.Fatalf("no connection file for the application to read:\n%s", seq)
	}
	if strings.Contains(string(rendered["postgres"]), "POSTGRES_PASSWORD=") {
		t.Fatal("the password must never appear in the generated runtime")
	}
}

func TestServiceApplyRefusesDestructiveMounts(t *testing.T) {
	// The running service uses a volume the planned document no longer names.
	f := accFake("volume=pgdata bind=/var/lib/ob/sample/releases/R0/conf")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.ServiceApply(context.Background(), "R9-acc", false)
	if err == nil || !strings.Contains(err.Error(), "pgdata") {
		t.Fatalf("want destructive refusal naming pgdata, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "ob_sample_postgres' -f") {
		t.Fatal("must not converge after refusal")
	}
	// A per-release payload bind changes every release by construction and is
	// not data.
	if strings.Contains(err.Error(), "/releases/") {
		t.Fatalf("release-relative bind wrongly flagged: %v", err)
	}

	f2 := accFake("volume=pgdata")
	e2 := New(testConfig(), testProject(t), f2, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e2.ServiceApply(context.Background(), "R9-acc", true); err != nil {
		t.Fatalf("force apply: %v", err)
	}
}

// A job needs a database more surely than anything else in the project, and the
// alias collection once walked only the release order — which excludes jobs, so
// a migration job's connection file was never written and the deploy failed on
// a path nobody had been told to create.
func TestAJobGetsItsConnectionFile(t *testing.T) {
	cfg := testConfig()
	migrate := cfg.Workloads["migrate"]
	migrate.Needs = []app.Need{{Name: "postgres", Condition: "started",
		Env: map[string]string{"DB_URL": "url"}}}
	cfg.Workloads["migrate"] = migrate

	f := accFake("")
	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.EnsureServiceConnections(context.Background()); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "postgres.migrate.env") {
		t.Fatalf("the job's connection file was never written:\n%s", seq)
	}
	if !strings.Contains(seq, "DB_URL") {
		t.Fatalf("the job's own variable name never reached the target:\n%s", seq)
	}
}

// A major version change to a service that cannot read the previous version's
// data directory is refused before anything is replaced. The diff shows one
// line — postgres:16 becoming postgres:17 — which reads as routine and is not.
func TestAnUnsafeMajorUpgradeIsRefusedBeforeConverging(t *testing.T) {
	cfg := testConfig()
	svc := cfg.Services["postgres"]
	svc.Version = "17"
	cfg.Services["postgres"] = svc
	for _, allowDestructiveMounts := range []bool{false, true} {
		t.Run(map[bool]string{false: "without mount override", true: "with mount override"}[allowDestructiveMounts], func(t *testing.T) {
			f := accFake("")
			base := f.Dynamic
			f.Dynamic = func(cmd string) (transport.Result, bool) {
				if strings.Contains(cmd, "postgres.version") {
					return transport.Result{Stdout: "16\n"}, true
				}
				return base(cmd)
			}
			e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
			err := e.ServiceApply(context.Background(), "R9", allowDestructiveMounts)
			if err == nil {
				t.Fatal("a major version change across an unreadable data directory must be refused")
			}
			if !strings.Contains(err.Error(), "cannot be opened") {
				t.Fatalf("the refusal must say what would happen: %v", err)
			}
			if strings.Contains(strings.Join(f.Commands, "\n"), "ob_sample_postgres' -f") {
				t.Fatal("it must refuse before replacing the container")
			}
		})
	}
}

// The version Onebox judges from is the one that last ran successfully, not
// the image that happens to be on the container. After a failed upgrade those
// differ, and judging from the image traps the operator on the way back.
func TestRecoveryIsNotBlockedWhenNoVersionWasRecorded(t *testing.T) {
	f := accFake("")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "postgres.version") {
			return transport.Result{Stdout: "\n"}, true // never recorded
		}
		return base(cmd)
	}
	cfg := testConfig()
	svc := cfg.Services["postgres"]
	svc.Version = "17"
	cfg.Services["postgres"] = svc

	e := New(cfg, testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	if err := e.ServiceApply(context.Background(), "R9", false); err != nil {
		t.Fatalf("with no recorded version Onebox cannot know what the data is, and must not guess: %v", err)
	}
}

// A durable volume whose credential is gone cannot be opened by a freshly
// generated one: the password is baked into the data directory. The service
// would start, report healthy, and refuse every connection the application
// makes — which surfaces four minutes later as "the container never became
// healthy" and names nothing useful.
func TestAVolumeWithoutItsCredentialIsRefused(t *testing.T) {
	f := accFake("")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "postgres.secret.env") && strings.Contains(cmd, "test -f") {
			return transport.Result{Stdout: ""}, true // no credential
		}
		if strings.Contains(cmd, "volume ls -q") && strings.Contains(cmd, "ob_sample_postgres_data") {
			return transport.Result{Stdout: "ob_sample_postgres_data\n"}, true // data is there
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production"})
	err := e.EnsureServiceConnections(context.Background())
	if err == nil {
		t.Fatal("data that no credential can open must be refused, not deployed against")
	}
	if !strings.Contains(err.Error(), "docker volume rm") {
		t.Fatalf("the refusal must name the way out: %v", err)
	}
}
