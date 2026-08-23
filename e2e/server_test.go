package e2e

import (
	"encoding/base64"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireDocker puts a container runtime on the server.
//
// ce7e41b made this the operator's job, done through the bootstrap hook, and
// the permutations that belong to that contract — no hook on a bare machine,
// a hook that fails — are their own tests. This is the happy path standing in
// for them so the backup coverage below can run.
func (s *server) requireDocker(t *testing.T) {
	t.Helper()
	if err := s.try(t, "command -v docker && docker compose version && docker buildx version"); err == nil {
		return
	}
	// Docker's own packages, not Ubuntu's.
	//
	// `apt install docker.io docker-buildx` looks equivalent and is not: that
	// buildx (0.30.1-0ubuntu1) accepts `--format` on `imagetools inspect` and
	// ignores it, printing the human-readable manifest and exiting 0. ob pins
	// every workload image through exactly that command
	// (internal/engine/plan.go), so on such a host no deploy can succeed — and
	// the error it produces blames the registry.
	//
	// This installs what an operator following Docker's instructions gets,
	// which is the configuration the product is really used in. The Ubuntu
	// packaging is a real gap and belongs in an issue, not in a fixture that
	// quietly avoids it.
	s.run(t, strings.Join([]string{
		"set -e",
		"export DEBIAN_FRONTEND=noninteractive",
		"apt-get remove -y -qq docker.io docker-buildx containerd runc >/dev/null 2>&1 || true",
		"apt-get update -qq",
		"apt-get install -y -qq ca-certificates curl >/dev/null",
		"install -m 0755 -d /etc/apt/keyrings",
		"curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc",
		"chmod a+r /etc/apt/keyrings/docker.asc",
		`echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] ` +
			`https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" ` +
			`> /etc/apt/sources.list.d/docker.list`,
		"apt-get update -qq",
		"apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null",
		// A pull-through cache for Docker Hub.
		//
		// ob resolves a tag through the registry on purpose — a tag can move,
		// and internal/engine/backup_postgres.go says so where it skips the
		// pull for a digest. But Hub rate-limits anonymous requests per source
		// address, and a CI runner shares its address with every other job on
		// it, so without a mirror this suite fails with 429s that are nobody's
		// bug. Pointing the daemon at a cache is what an operator behind one
		// does, and it leaves ob's resolution behaviour untouched.
		"mkdir -p /etc/docker",
		`printf '{\n  "registry-mirrors": ["https://mirror.gcr.io"]\n}\n' > /etc/docker/daemon.json`,
		"systemctl enable --now docker.socket docker",
		// Restarted explicitly: installing the package already started the
		// daemon, so the configuration written a moment ago is not the one it
		// is running under until it is told to read it again.
		"systemctl restart docker",
		"docker info --format '{{json .RegistryConfig.Mirrors}}' | grep -q mirror.gcr.io",
	}, "\n"))
}

// primeDriverImage puts the PostgreSQL image on the server from a mirror.
//
// The driver names `postgres:18` and the tag is not overridable, so every run
// would otherwise pull it from Docker Hub — which rate-limits anonymous
// requests per source address, and a CI runner shares its address with
// everyone else on it. The image content is identical across registries, so
// retagging it locally is the same bytes by the same digest: `docker image
// inspect` reports postgres@sha256:… first, which is what ob reads when it
// pins the protected image.
func (s *server) primeDriverImage(t *testing.T) {
	t.Helper()
	if err := s.try(t, "docker image inspect postgres:18 >/dev/null 2>&1"); err == nil {
		return
	}
	s.run(t, "docker pull -q public.ecr.aws/docker/library/postgres:18 >/dev/null && "+
		"docker tag public.ecr.aws/docker/library/postgres:18 postgres:18")
}

// psql runs a query in the protected server and returns the single value.
func (s *server) psql(t *testing.T, query string) string {
	t.Helper()
	container := strings.TrimSpace(s.run(t,
		`docker ps -q --filter label=com.docker.compose.service=postgres | head -1`))
	if container == "" {
		t.Fatal("no postgres container is running")
	}
	// The query travels base64-encoded so that quoting it through ssh, then a
	// shell, then docker exec, then psql cannot change what PostgreSQL runs.
	// SQL is full of quotes and this test has three shells between it and the
	// server.
	//
	// The superuser and database are ob's, not PostgreSQL's defaults: it
	// generates the credentials and passes them to the image, so the container
	// itself is the only authority on what they are.
	encoded := base64.StdEncoding.EncodeToString([]byte(query))
	return strings.TrimSpace(s.run(t,
		`docker exec `+container+` sh -c 'echo `+encoded+` | base64 -d | psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tA'`))
}

// rotateWAL closes the current segment so there is something to archive.
//
// pg_switch_wal() alone is not enough: PostgreSQL skips the switch when
// nothing has been written since the last one, so on a quiet database it
// returns without producing a segment and a test waiting for the count to move
// waits forever against a perfectly healthy archiver.
func (s *server) rotateWAL(t *testing.T) {
	t.Helper()
	s.psql(t, "create table if not exists churn(n int)")
	s.psql(t, "insert into churn select generate_series(1, 20000)")
	s.psql(t, "select pg_switch_wal()")
}

// archiverCounts reads what the archiver has actually shipped. This is the
// assertion the backup tests turn on: it is a fact about the repository rather
// than about how ob arranged the container, so it stays true whatever shape
// the fix for a broken upload path takes.
func (s *server) archiverCounts(t *testing.T) (archived, failed int) {
	t.Helper()
	row := s.psql(t, "select archived_count || ' ' || failed_count from pg_stat_archiver")
	fields := strings.Fields(row)
	if len(fields) != 2 {
		t.Fatalf("unreadable pg_stat_archiver row: %q", row)
	}
	archived, _ = strconv.Atoi(fields[0])
	failed, _ = strconv.Atoi(fields[1])
	return archived, failed
}

// serves is what the application workload is currently answering with.
//
// Read from inside the container rather than through a published port: the
// fixture declares no proxy, and what matters is which release is running, not
// how the machine exposes it.
func (s *server) serves(t *testing.T) string {
	t.Helper()
	container := strings.TrimSpace(s.run(t,
		`docker ps -q --filter label=com.docker.compose.service=app | head -1`))
	if container == "" {
		t.Fatal("no application container is running")
	}
	return strings.TrimSpace(s.run(t, `docker exec `+container+` wget -qO- http://localhost:8080/`))
}

// TestServerLifecycle walks the product the way an operator does: one machine,
// first contact to teardown, in order.
//
// The subtests are steps rather than independent cases, and they share the
// server and the release deliberately — a lifecycle is the thing under test,
// and rebuilding it per case would cost minutes per run and would still not
// prove that the steps compose. Ordering is Go's: subtests run in the order
// they are declared.
func TestServerLifecycle(t *testing.T) {
	s := requireServer(t)
	s.requireDocker(t)
	s.primeDriverImage(t)
	endpoint := s.objectStore(t)
	dir := s.project(t, endpoint, "v1")

	// preflight changes nothing, so it reports rather than refuses — and what
	// it reports about an unclaimed machine is the thing worth asserting: an
	// operator runs this before bootstrap precisely to be told what is missing.
	t.Run("preflight reports an unclaimed host", func(t *testing.T) {
		out := s.mustOb(t, dir, "preflight")
		if !strings.Contains(out, "unclaimed") {
			t.Fatalf("preflight did not report the host as unclaimed before bootstrap:\n%s", out)
		}
	})

	t.Run("bootstrap and first release", func(t *testing.T) {
		s.deploy(t, dir)
		if body := s.serves(t); body != "v1" {
			t.Fatalf("the workload serves %q, want v1", body)
		}
	})

	t.Run("scheduled jobs are bounded and failures reach status", func(t *testing.T) {
		s.run(t, "systemd-analyze verify "+
			"/etc/systemd/system/ob-observer-chore.service "+
			"/etc/systemd/system/ob-observer-chore.timer "+
			"/etc/systemd/system/ob-observer-timeout--chore.service "+
			"/etc/systemd/system/ob-observer-timeout--chore.timer")

		// Model a host last touched by v2026.8.5: the timer exists, but its
		// service invokes Compose directly and has no bounded runner or notifier.
		// Upgrading the local package is intentionally side-effect free; the
		// scoped apply command must bridge that installed generation without an
		// unrelated release deploy.
		legacyService := `[Unit]
Description=Onebox scheduled job chore for observer
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/bin/docker compose -p observer -f /var/lib/ob/observer/current/compose.yaml run --rm --no-deps chore
`
		encodedLegacy := base64.StdEncoding.EncodeToString([]byte(legacyService))
		s.run(t, strings.Join([]string{
			"printf '%s' '" + encodedLegacy + "' | base64 -d > /etc/systemd/system/ob-observer-chore.service",
			"rm -f /etc/systemd/system/ob-observer-chore.run /etc/systemd/system/ob-observer-chore.notify",
			"systemctl daemon-reload",
		}, "\n"))
		before := s.run(t, "systemctl cat ob-observer-chore.service")
		if strings.Contains(before, "TimeoutStartSec=") || strings.Contains(before, "ExecStopPost=") {
			t.Fatalf("legacy fixture already has the current unit contract:\n%s", before)
		}
		s.mustOb(t, dir, "schedule", "apply")
		after := s.run(t, strings.Join([]string{
			"test -s /etc/systemd/system/ob-observer-chore.run",
			"test -s /etc/systemd/system/ob-observer-chore.notify",
			"systemctl cat ob-observer-chore.service",
		}, "\n"))
		for _, want := range []string{"ExecStart=/bin/sh", "ExecStopPost=/bin/sh", "TimeoutStartSec="} {
			if !strings.Contains(after, want) {
				t.Fatalf("schedule apply did not restore %q:\n%s", want, after)
			}
		}

		// A normal host-fired run proves the generated runner, current-release
		// lookup, Docker invocation and app-wide schedule lock compose on systemd.
		s.run(t, "systemctl start ob-observer-chore.service")
		if result := strings.TrimSpace(s.run(t,
			"systemctl show ob-observer-chore.service --property=Result --value")); result != "success" {
			t.Fatalf("normal scheduled run result = %q, want success", result)
		}

		// The receiver lives on the target because host-fired notifications do
		// too. It accepts one POST, records the body, and exits.
		receiver := `from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        with open("/tmp/onebox-schedule-notify", "wb") as out:
            out.write(self.rfile.read(size))
        self.send_response(204)
        self.end_headers()
    def log_message(self, format, *args):
        pass
HTTPServer(("127.0.0.1", 18080), Handler).handle_request()
`
		encoded := base64.StdEncoding.EncodeToString([]byte(receiver))
		s.run(t, strings.Join([]string{
			"set -e",
			"command -v python3 >/dev/null",
			"systemctl stop ob-e2e-schedule-receiver.service >/dev/null 2>&1 || true",
			"systemctl reset-failed ob-e2e-schedule-receiver.service >/dev/null 2>&1 || true",
			"rm -f /tmp/onebox-schedule-notify",
			"printf '%s' '" + encoded + "' | base64 -d > /tmp/onebox-schedule-receiver.py",
			"systemd-run --quiet --collect --unit=ob-e2e-schedule-receiver /usr/bin/python3 /tmp/onebox-schedule-receiver.py",
			"for i in $(seq 1 50); do ss -ltn | grep -q '127.0.0.1:18080' && break; sleep .1; done",
			"ss -ltn | grep -q '127.0.0.1:18080'",
		}, "\n"))

		// systemctl returns non-zero because TimeoutStartSec terminates the job.
		if err := s.try(t, "systemctl start ob-observer-timeout--chore.service"); err == nil {
			t.Fatal("wedged scheduled job was not terminated by its timeout")
		}
		if result := strings.TrimSpace(s.run(t,
			"systemctl show ob-observer-timeout--chore.service --property=Result --value")); result != "timeout" {
			t.Fatalf("timed-out scheduled run result = %q, want timeout", result)
		}
		out, err := s.ob(t, dir, "status")
		if err == nil || !strings.Contains(out, "schedule timeout-chore") || !strings.Contains(out, "last run failed: timeout") {
			t.Fatalf("status did not expose the scheduled failure (err=%v):\n%s", err, out)
		}
		notification := strings.TrimSpace(s.run(t, "cat /tmp/onebox-schedule-notify"))
		for _, want := range []string{
			`"app":"observer"`,
			`"verb":"scheduled job timeout-chore"`,
			`"status":"fail"`,
			`"error":"operation failed; inspect trusted local diagnostics"`,
		} {
			if !strings.Contains(notification, want) {
				t.Fatalf("scheduled failure notification is missing %q: %s", want, notification)
			}
		}
		// Do not make the deliberately induced failure pollute later lifecycle
		// assertions; systemd resets Result to success with the failed state.
		s.run(t, "systemctl reset-failed ob-observer-timeout--chore.service")
	})

	t.Run("preflight", func(t *testing.T) {
		s.mustOb(t, dir, "preflight")
	})

	t.Run("status reports the release", func(t *testing.T) {
		out := s.mustOb(t, dir, "status")
		for _, want := range []string{"app", "postgres"} {
			if !strings.Contains(out, want) {
				t.Errorf("status never mentions %q:\n%s", want, out)
			}
		}
	})

	t.Run("exec reaches the workload", func(t *testing.T) {
		out := s.mustOb(t, dir, "exec", "app", "--reason", "server e2e", "--", "cat", "/srv/index.html")
		if !strings.Contains(out, "v1") {
			t.Fatalf("exec did not run in the release:\n%s", out)
		}
	})

	t.Run("logs", func(t *testing.T) {
		s.mustOb(t, dir, "logs", "app")
	})

	t.Run("audit records the release", func(t *testing.T) {
		out := s.mustOb(t, dir, "audit")
		if strings.TrimSpace(out) == "" {
			t.Fatal("audit is empty after a successful deploy")
		}
	})

	t.Run("second release", func(t *testing.T) {
		s.render(t, dir, endpoint, "v2")
		s.deploy(t, dir)
		if body := s.serves(t); body != "v2" {
			t.Fatalf("the workload serves %q after the second release, want v2", body)
		}
	})

	t.Run("rollback returns the previous release", func(t *testing.T) {
		s.mustOb(t, dir, "rollback")
		if body := s.serves(t); body != "v1" {
			t.Fatalf("the workload serves %q after rollback, want v1", body)
		}
	})

	t.Run("service apply converges the data service", func(t *testing.T) {
		s.mustOb(t, dir, "service", "apply")
	})

	t.Run("secrets list", func(t *testing.T) {
		s.mustOb(t, dir, "secrets", "list")
	})

	t.Run("job plan and run", func(t *testing.T) {
		plan := filepath.Join(dir, "ob-job-plan.json")
		s.mustOb(t, dir, "job", "plan", "chore", "-o", plan)
		out, err := s.obInput(t, dir, s.obHome(t), "y\n", "job", "run", "--plan", plan)
		if err != nil {
			t.Fatalf("job run failed: %v\n%s", err, out)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		s.mustOb(t, dir, "doctor")
	})

	// Issue #88: enable established archiving and then failed every upload
	// with "x509: certificate signed by unknown authority", because wal-g runs
	// inside the driver's image and postgres:18 carries no certificate
	// authorities. It failed after the base backup, leaving archiving on.
	//
	// This endpoint is signed by an authority that exists only on this server,
	// so it can only verify if ob carries the host trust store into the
	// container the way it already carries the binary.
	t.Run("backup enable archives to a privately trusted endpoint", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "enable", "postgres")
		s.rotateWAL(t)

		deadline := time.Now().Add(90 * time.Second)
		for {
			archived, failed := s.archiverCounts(t)
			if failed > 0 {
				t.Fatalf("the archiver is failing (%d archived, %d failed); wal-g cannot reach %s",
					archived, failed, endpoint)
			}
			if archived >= 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("nothing reached the repository: %d archived, %d failed", archived, failed)
			}
			time.Sleep(2 * time.Second)
		}
	})

	t.Run("backup status reads the repository", func(t *testing.T) {
		out := s.mustOb(t, dir, "backup", "status", "postgres")
		if strings.TrimSpace(out) == "" {
			t.Fatal("backup status says nothing about a protected service")
		}
	})

	t.Run("backup create takes another base backup", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "create", "postgres")
	})

	t.Run("backup verify proves the chain", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "verify", "postgres")
	})

	t.Run("backup drill recovers without touching the database", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "drill", "postgres")
		if body := s.serves(t); body != "v1" {
			t.Fatalf("a drill disturbed the live release: serving %q", body)
		}
	})

	t.Run("backup restore returns the data", func(t *testing.T) {
		s.psql(t, "create table if not exists survivors(note text)")
		s.psql(t, "insert into survivors values (concat('written', ' ', 'before'))")
		s.mustOb(t, dir, "backup", "create", "postgres")
		s.mustOb(t, dir, "backup", "restore", "postgres", "--confirm", "postgres")
		if note := s.psql(t, "select note from survivors limit 1"); note != "written before" {
			t.Fatalf("the recovered cluster does not hold the row: %q", note)
		}
	})

	t.Run("backup prune", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "prune", "postgres")
	})

	t.Run("backup disable stops archiving and removes its timers", func(t *testing.T) {
		s.mustOb(t, dir, "backup", "disable", "postgres", "--confirm", "postgres")
		if mode := s.psql(t, "show archive_mode"); mode == "on" {
			t.Error("archiving is still on after disable")
		}
		units := s.run(t, "systemctl list-units --type=timer --all --no-pager | grep -c ob-backup || true")
		if strings.TrimSpace(units) != "0" {
			t.Errorf("backup timers survived disable: %s", units)
		}
	})

	// Last, because it removes what every step above built.
	t.Run("destroy leaves the machine clean", func(t *testing.T) {
		out, err := s.obInput(t, dir, s.obHome(t), "observer\ny\n", "destroy", "--volumes")
		if err != nil {
			t.Fatalf("destroy failed: %v\n%s", err, out)
		}
		if left := strings.TrimSpace(s.run(t,
			`docker ps -aq --filter label=ob.app=observer | wc -l`)); left != "0" {
			t.Errorf("%s containers survived destroy", left)
		}
		// The object store is not ob's and must be untouched by a teardown of
		// the application beside it.
		if err := s.try(t, "systemctl is-active --quiet minio"); err != nil {
			t.Error("destroy stopped a service that does not belong to ob")
		}
	})
}
