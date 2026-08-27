package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func opsFake(remoteSecretsHash string) *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R7\n"}, true
		}
		if strings.Contains(cmd, "/releases/R7/ob.snapshot.yml") {
			return transport.Result{Stdout: engineProject}, true
		}
		if strings.Contains(cmd, "sha256sum") {
			return transport.Result{Stdout: remoteSecretsHash + "\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestDestroySequence(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "down --remove-orphans") {
		t.Fatalf("compose down missing:\n%s", seq)
	}
	if strings.Contains(seq, "down --remove-orphans -v") {
		t.Fatal("volumes must be kept without --volumes")
	}
	if !strings.Contains(seq, "! -name services") {
		t.Fatalf("state dir not removed:\n%s", seq)
	}
	// Volumes are kept, so the credentials that open them are kept too. The
	// alternative is data nobody can ever read again.
	if strings.Contains(seq, "rm -rf '/var/lib/ob/sample'") {
		t.Fatalf("kept volumes lost their credentials:\n%s", seq)
	}
	if !strings.Contains(seq, "systemctl disable --now ob-sample-") &&
		strings.Contains(seq, "list-unit-files") {
		// no timers installed in this fixture; the sweep still has to run
		if !strings.Contains(seq, "list-unit-files --no-legend --type=timer") {
			t.Fatalf("schedules not swept:\n%s", seq)
		}
	}
}

// Destroying with --volumes removes the data, so keeping the credential that
// opens it would serve nobody.
func TestDestroyWithVolumesRemovesEverything(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), true, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -rf '/var/lib/ob/sample'") {
		t.Fatalf("state dir not removed:\n%s", seq)
	}
	if !strings.Contains(seq, "rm -f '/var/lib/ob/_host/owner'") {
		t.Fatalf("complete teardown without a managed proxy retained host ownership:\n%s", seq)
	}
	for _, network := range []string{"sample_default", "ob_sample"} {
		if !strings.Contains(seq, "docker network rm '"+network+"'") {
			t.Fatalf("complete teardown retained network %s:\n%s", network, seq)
		}
	}
}

func TestDestroyStopsBeforeStateRemovalWhenNetworkHasEndpoints(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker network rm 'sample_default'") {
			return transport.Result{ExitCode: 1, Stderr: "network has active endpoints"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Destroy(context.Background(), true, false)
	if err == nil || !strings.Contains(err.Error(), "detach its remaining endpoints") {
		t.Fatalf("destroy endpoint error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "rm -rf '/var/lib/ob/sample'") || strings.Contains(seq, "rm -f '/var/lib/ob/_host/owner'") {
		t.Fatalf("destroy discarded recovery state after network removal failed:\n%s", seq)
	}
}

func TestDestroyStopsBeforeStateRemovalWhenNetworkInspectFails(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "docker network inspect") && strings.Contains(command, "sample_default") {
			return transport.Result{ExitCode: 1, Stderr: "permission denied"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Destroy(context.Background(), true, false)
	if err == nil || !strings.Contains(err.Error(), "cannot inspect ownership") {
		t.Fatalf("destroy inspect error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "rm -rf '/var/lib/ob/sample'") || strings.Contains(seq, "rm -f '/var/lib/ob/_host/owner'") {
		t.Fatalf("destroy discarded recovery state after network inspection failed:\n%s", seq)
	}
}

// Teardown belongs to the release being removed, not the project currently in
// the working tree. A migration may have replaced a Compose-defined database
// with a managed service and removed the interpolation file the old Compose
// document still needs merely to parse.
func TestDestroyUsesTheCurrentReleaseEnvironment(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/releases/R7/ob.snapshot.yml") {
			return transport.Result{Stdout: engineProject + "\nruntime:\n  env_files: [legacy.env]\n"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), true, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	commands := strings.Join(f.Commands, "\n")
	want := "--env-file '/var/lib/ob/sample/releases/R7/legacy.env' down --remove-orphans -v"
	if !strings.Contains(commands, want) {
		t.Fatalf("destroy did not use the current release's interpolation environment; want %q:\n%s", want, commands)
	}
}

func TestDestroyRefusesMissingCurrentReleaseSnapshot(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "/releases/R7/ob.snapshot.yml") {
			return transport.Result{ExitCode: 1, Stderr: "not found"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.Destroy(context.Background(), true, false)
	if err == nil || !strings.Contains(err.Error(), "destroy refused: release R7 snapshot unavailable") {
		t.Fatalf("destroy error = %v", err)
	}
	commands := strings.Join(f.Commands, "\n")
	if strings.Contains(commands, "down --remove-orphans") || strings.Contains(commands, "docker volume rm") || strings.Contains(commands, "rm -rf '/var/lib/ob/sample'") {
		t.Fatalf("destroy mutated release state without its snapshot:\n%s", commands)
	}
}

func TestDestroyReleasesAppLockOnEarlyFailure(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(command string) (transport.Result, bool) {
		if strings.Contains(command, "> '/var/lib/ob/sample/fence'") {
			return transport.Result{ExitCode: 70, Stderr: "fence is read-only"}, true
		}
		return base(command)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err == nil {
		t.Fatal("destroy succeeded after fence failure")
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"), "rm -f '/var/lib/ob/sample/lock'") {
		t.Fatalf("destroy retained app lock after early failure:\n%s", strings.Join(f.Commands, "\n"))
	}
}

func TestLogsAndExecShapes(t *testing.T) {
	f := opsFake("x")
	var out bytes.Buffer
	cfg := testConfig()
	e := New(cfg, testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Logs(context.Background(), "web", true, 50, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "logs --tail 50 --follow web") {
		t.Fatalf("logs shape wrong:\n%s", seq)
	}
	if err := e.Logs(context.Background(), "postgres", false, 20, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	seq = strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker compose -p ob_sample_postgres -f '/var/lib/ob/sample/services/postgres.yaml' logs --tail 20 postgres") {
		t.Fatalf("service logs shape wrong:\n%s", seq)
	}
	if _, err := e.ExecInAudited(context.Background(), "exec-workload", "web", "alembic current", "inspect migration state", &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	seq = strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker exec OLD1 sh -c 'alembic current'") {
		t.Fatalf("exec shape wrong:\n%s", seq)
	}
	if !strings.Contains(seq, `cat '/var/lib/ob/sample/fence'`) || !strings.Contains(seq, `then docker exec OLD1`) {
		t.Fatalf("exec is not guarded by the acquired mutation fence:\n%s", seq)
	}
	if _, err := e.ExecInAudited(context.Background(), "exec-service", "postgres", "psql --version", "verify client version", &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	seq = strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker exec PG1 sh -c 'psql --version'") {
		t.Fatalf("workload exec shape wrong:\n%s", seq)
	}
	if target, err := e.ResolveRuntimeTarget("postgres"); err != nil || target.Kind != RuntimeTargetService {
		t.Fatalf("service resolution = %#v, %v", target, err)
	}
	if _, err := e.ResolveRuntimeTarget("nope"); err == nil || !strings.Contains(err.Error(), "postgres (service)") || !strings.Contains(err.Error(), "web (workload)") {
		t.Fatalf("unknown target error = %v", err)
	}
}

func TestExecAuditPersistsDigestButNeverCommandOrOutput(t *testing.T) {
	f := opsFake("x")
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "docker exec ") {
			return transport.Result{Stdout: "passthrough-secret-value\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: io.Discard, Sleep: noSleep})
	command := "printf command-secret-value"
	var stdout bytes.Buffer
	if _, err := e.ExecInAudited(context.Background(), "exec-redaction", "web", command, "incident 42 inspection", &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "passthrough-secret-value\n" {
		t.Fatalf("passthrough = %q", stdout.String())
	}
	var journalCommands []string
	for _, cmd := range f.Commands {
		if strings.Contains(cmd, "/journal/exec-redaction.jsonl") {
			journalCommands = append(journalCommands, cmd)
		}
	}
	journalText := strings.Join(journalCommands, "\n")
	if len(journalCommands) != 2 || !strings.Contains(journalText, HashBytes([]byte(command))) || !strings.Contains(journalText, "incident 42 inspection") {
		t.Fatalf("journal evidence = %s", journalText)
	}
	if strings.Contains(journalText, "command-secret-value") || strings.Contains(journalText, "passthrough-secret-value") {
		t.Fatalf("journal persisted exec bytes: %s", journalText)
	}
}

func TestExecAuditRecordsFailureAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "failure", err: errors.New("container failed"), code: "exec_failed"},
		{name: "cancelled", err: context.Canceled, code: "exec_cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := opsFake("x")
			f.Err = func(cmd string) error {
				if strings.Contains(cmd, "docker exec ") {
					return test.err
				}
				return nil
			}
			e := New(testConfig(), testProject(t), f, Options{Out: io.Discard, Sleep: noSleep})
			_, err := e.ExecInAudited(context.Background(), "exec-"+test.name, "web", "false secret-command", "test terminal audit", io.Discard, io.Discard)
			if !errors.Is(err, test.err) {
				t.Fatalf("exec error = %v", err)
			}
			var journalText string
			for _, cmd := range f.Commands {
				if strings.Contains(cmd, "/journal/exec-"+test.name+".jsonl") {
					journalText += cmd
				}
			}
			if !strings.Contains(journalText, test.code) || strings.Contains(journalText, "secret-command") {
				t.Fatalf("terminal journal = %s", journalText)
			}
		})
	}
}

func TestExecReasonRefusedBeforeTargetContact(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: io.Discard, Sleep: noSleep})
	for _, reason := range []string{"", "line one\nline two", strings.Repeat("x", maxExecReasonBytes+1)} {
		if _, err := e.ExecInAudited(context.Background(), "exec-invalid", "web", "true", reason, io.Discard, io.Discard); err == nil {
			t.Fatalf("reason %q accepted", reason)
		}
	}
	if len(f.Commands) != 0 {
		t.Fatalf("invalid reasons contacted target: %v", f.Commands)
	}
}

func proxyManagedCfg() *app.Resolved {
	cfg := testConfig()
	cfg.Proxy = app.Proxy{Kind: "traefik-docker", Managed: true, Config: "traefik"}
	return cfg
}

func TestDestroyKeepsHostProxyWithoutFlag(t *testing.T) {
	f := opsFake("x")
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "/proxy/apps") {
		t.Fatalf("destroy must not consult a cross-application proxy registry:\n%s", seq)
	}
	if strings.Contains(seq, "-p onebox-proxy -f '/var/lib/ob/_host/proxy/compose.yaml' down") {
		t.Fatalf("without --proxy the host proxy must survive:\n%s", seq)
	}
}

func TestDestroyProxyTeardownForSoleOwner(t *testing.T) {
	f := opsFake("x")
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, true); err != nil {
		t.Fatalf("destroy --proxy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker compose -p onebox-proxy -f '/var/lib/ob/_host/proxy/compose.yaml' down") {
		t.Fatalf("sole owner with --proxy must tear the proxy down:\n%s", seq)
	}
	for _, selector := range []string{
		"name=^onebox-proxy$ --filter label=com.docker.compose.project=onebox-proxy --filter label=com.docker.compose.service=proxy",
		"name=^onebox-discovery$ --filter label=com.docker.compose.project=onebox-proxy --filter label=com.docker.compose.service=discovery",
		"name=^onebox-proxy-discovery$ --filter label=com.docker.compose.project=onebox-proxy --filter label=com.docker.compose.service=discovery",
	} {
		if !strings.Contains(seq, selector) {
			t.Fatalf("proxy teardown must sweep owned orphan %s even when Compose state is missing:\n%s", selector, seq)
		}
	}
	if !strings.Contains(seq, "rm -rf '/var/lib/ob/_host/proxy'") {
		t.Fatalf("proxy state dir must go with it:\n%s", seq)
	}
}

func TestCompleteDestroyReleasesHostOwnership(t *testing.T) {
	f := opsFake("x")
	e := New(proxyManagedCfg(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), true, true); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "rm -f '/var/lib/ob/_host/owner'") {
		t.Fatalf("complete teardown must release the sole owner record:\n%s", seq)
	}
}

func TestDestroyProxyFlagRequiresManaged(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, true); err == nil {
		t.Fatal("--proxy without proxy.managed must error")
	}
}

func TestDestroyVolumesOnSweepPath(t *testing.T) {
	// no release ever activated (bootstrap-only host): teardown sweeps by
	// label — --volumes must still remove the project's named volumes
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: ""}, true // never activated
		}
		if strings.Contains(cmd, "docker ps -aq") {
			return transport.Result{Stdout: "C1\n"}, true
		}
		if strings.Contains(cmd, "docker volume ls") {
			return transport.Result{Stdout: "sample_pgdata\nsample_cache\n"}, true
		}
		return base(cmd)
	}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), true, false); err != nil {
		t.Fatalf("destroy: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker volume rm sample_pgdata sample_cache") {
		t.Fatalf("--volumes on the sweep path must remove labeled volumes:\n%s", seq)
	}

	// and WITHOUT --volumes the sweep must not touch them
	f2 := happyFake()
	base2 := f2.Dynamic
	f2.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: ""}, true
		}
		if strings.Contains(cmd, "docker ps -aq") {
			return transport.Result{Stdout: "C1\n"}, true
		}
		return base2(cmd)
	}
	e2 := New(testConfig(), testProject(t), f2, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e2.Destroy(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f2.Commands, "\n"), "docker volume") {
		t.Fatalf("volumes must be kept without --volumes:\n%s", strings.Join(f2.Commands, "\n"))
	}
}

func TestRemoveServicesRemovesPreservedRestoreVolumes(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "docker ps -aq"):
			return transport.Result{}, true
		case strings.Contains(cmd, "docker volume ls") && strings.Contains(cmd, "label=com.docker.compose.project"):
			return transport.Result{Stdout: "ob_sample_postgres_data\n"}, true
		case strings.Contains(cmd, "docker volume ls") && strings.Contains(cmd, "before-restore"):
			return transport.Result{Stdout: strings.Join([]string{
				"ob_sample_postgres_data-before-restore-20260822T160242Z",
				"ob_sample_postgres_data-before-restore-not-a-timestamp",
				"ob_other_postgres_data-before-restore-20260822T160242Z",
			}, "\n")}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	e.fenceVal = "destroy 1"
	if err := e.removeServices(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "docker volume rm ob_sample_postgres_data ob_sample_postgres_data-before-restore-20260822T160242Z") {
		t.Fatalf("destroy did not remove the live and preserved volumes:\n%s", seq)
	}
	if strings.Contains(seq, "docker volume rm ob_other") || strings.Contains(seq, "docker volume rm ob_sample_postgres_data-before-restore-not") {
		t.Fatalf("destroy removed a volume whose ownership was not proved:\n%s", seq)
	}
}

func TestDestroyRefusesFailedSweepDiscovery(t *testing.T) {
	for _, test := range []struct {
		name          string
		removeVolumes bool
		failureMatch  string
		want          string
	}{
		{name: "containers", failureMatch: "docker ps -aq", want: "list application containers failed"},
		{name: "volumes", removeVolumes: true, failureMatch: "docker volume ls", want: "list application volumes failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := happyFake()
			base := f.Dynamic
			f.Dynamic = func(command string) (transport.Result, bool) {
				switch {
				case strings.Contains(command, "readlink"):
					return transport.Result{}, true
				case strings.Contains(command, test.failureMatch):
					return transport.Result{ExitCode: 42, Stderr: "daemon unavailable"}, true
				case strings.Contains(command, "docker ps -aq"):
					return transport.Result{}, true
				default:
					return base(command)
				}
			}
			e := New(testConfig(), testProject(t), f, Options{Out: io.Discard, Sleep: noSleep})
			err := e.Destroy(context.Background(), test.removeVolumes, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("destroy error = %v, want %q", err, test.want)
			}
			if commands := strings.Join(f.Commands, "\n"); strings.Contains(commands, "rm -rf '/var/lib/ob/sample'") {
				t.Fatalf("destroy removed state after failed discovery:\n%s", commands)
			}
		})
	}
}

// Ownership gates every command, so releasing it while this application's data
// is still on the host locks the owner out of the only supported way to reclaim
// it — and if another application bootstraps in between, the volumes and the
// credentials that open them are stranded for good.
func TestDestroyKeepsHostOwnershipWhileDataRemains(t *testing.T) {
	f := opsFake("x")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if strings.Contains(seq, "rm -f '/var/lib/ob/_host/owner'") {
		t.Fatalf("ownership was released while volumes were kept:\n%s", seq)
	}
}

// With a managed proxy --proxy is always part of the releasing command, so the
// notice must name it even on the run that just passed --proxy and kept
// volumes. Keying the suffix on !removeProxy told that operator to run
// `ob destroy --volumes`, which retains ownership again — the exact defect the
// notice exists to prevent.
func TestDestroyNamesProxyEvenAfterAProxyOnlyDestroy(t *testing.T) {
	spec := testConfig()
	spec.Spec.Proxy.Managed = true
	f := opsFake("x")
	var out bytes.Buffer
	e := New(spec, testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, true); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !strings.Contains(out.String(), "run `ob destroy --volumes --proxy`") {
		t.Fatalf("retention notice named a command that retains ownership again:\n%s", out.String())
	}
}

func TestDestroyTellsTheOperatorHowToReleaseTheHost(t *testing.T) {
	f := opsFake("x")
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.Destroy(context.Background(), false, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// A retained record with no way to discover the remedy is the defect that
	// made releasing it early look attractive in the first place.
	// The exact command matters. This fixture's proxy is unmanaged, so --proxy
	// would be wrong here; a managed one needs it, and an operator following a
	// command missing a required flag retains ownership again.
	if !strings.Contains(out.String(), "run `ob destroy --volumes` to remove what remains") {
		t.Fatalf("retention notice did not name the releasing command:\n%s", out.String())
	}
	if strings.Contains(out.String(), "--volumes --proxy") {
		t.Fatalf("retention notice demanded --proxy for an unmanaged proxy:\n%s", out.String())
	}
}
