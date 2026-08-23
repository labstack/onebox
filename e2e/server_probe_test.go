package e2e

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Adversarial probes.
//
// The lifecycle suite walks the path an operator takes when everything works.
// These take the same machine and ask what ob leaves behind when it is
// interrupted, repeated, or pointed at something that is not there — which is
// where a tool that manages one box tends to be wrong, because the state it
// leaves is on a machine no unit test can see.
func TestServerProbes(t *testing.T) {
	s := requireServer(t)
	s.requireDocker(t)
	s.primeDriverImage(t)
	endpoint := s.objectStore(t)

	// An enablement that cannot reach the repository must leave the service
	// exactly as it found it. The failure path is the one that matters: it runs
	// after archiving has been turned on, and a service left archiving into
	// nothing retains every WAL segment it cannot ship.
	t.Run("an unreachable repository leaves archiving off", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)

		s.run(t, "systemctl stop minio")
		defer s.run(t, "systemctl start minio")

		out, err := s.ob(t, dir, "backup", "enable", "postgres")
		if err == nil {
			t.Fatalf("enable succeeded against a repository that is not running:\n%s", out)
		}
		if mode := s.psql(t, "show archive_mode"); mode == "on" {
			t.Error("the service is archiving into a repository it cannot reach")
		}
		timers := strings.TrimSpace(s.run(t,
			"systemctl list-units --type=timer --all --no-pager | grep -c ob-backup || true"))
		if timers != "0" {
			t.Errorf("a failed enablement left %s backup timer(s) installed", timers)
		}
		s.teardown(t, dir)
	})

	// The lifecycle suite enables backup on a database that has been running
	// for a while. This asks the same question of a cluster that has just been
	// created, which is what an operator gets when they enable backup on a
	// service they have only now deployed — the far more common case.
	t.Run("enable on a fresh cluster archives", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)
		s.mustOb(t, dir, "backup", "enable", "postgres")

		before, _ := s.archiverCounts(t)
		s.rotateWAL(t)
		deadline := time.Now().Add(60 * time.Second)
		for {
			archived, failed := s.archiverCounts(t)
			if archived > before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a freshly created cluster never archived: %d archived, %d failed, last failed %q",
					archived, failed, s.psql(t, "select last_failed_wal from pg_stat_archiver"))
			}
			time.Sleep(2 * time.Second)
		}
		s.teardown(t, dir)
	})

	// Re-running enable is the documented way to move a service to an edited
	// policy, so it has to be safe to run twice. It has been wrong before: an
	// earlier version deleted the credential file the same run had installed.
	t.Run("enable is safe to repeat", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)
		s.mustOb(t, dir, "backup", "enable", "postgres")
		s.mustOb(t, dir, "backup", "enable", "postgres")

		// Names.BackupCredentialFile: <app>/backup/secrets/<service>-<target>.env
		credentials := strings.TrimSpace(s.run(t,
			"ls /var/lib/ob/observer/backup/secrets 2>/dev/null | wc -l"))
		if credentials == "0" {
			t.Error("re-enabling removed the credential file it had just installed")
		}
		// Progress, not the cumulative failure counter. pg_stat_archiver counts
		// since the last stats reset and survives a restart, so an enablement
		// that restarts the server can leave failures in the history without
		// anything being wrong now — PostgreSQL retries what it could not ship.
		// What must be true is that the repository keeps advancing afterwards.
		before, _ := s.archiverCounts(t)
		s.rotateWAL(t)
		deadline := time.Now().Add(60 * time.Second)
		for {
			archived, _ := s.archiverCounts(t)
			if archived > before {
				break
			}
			if time.Now().After(deadline) {
				_, failed := s.archiverCounts(t)
				t.Fatalf("nothing reached the repository after a second enable: still %d archived, %d failed",
					archived, failed)
			}
			time.Sleep(2 * time.Second)
		}
		s.teardown(t, dir)
	})

	// The same repetition, with the archiver drained first.
	//
	// This distinguishes two explanations for #91. If the collision is between
	// `wal-g backup-push` and a segment PostgreSQL has not shipped yet, letting
	// the archiver catch up before the second enable removes it. If it survives
	// a drained archiver, the two writers collide over the segment being
	// written during the backup itself, and the fix has to be elsewhere.
	t.Run("enable repeats cleanly once the archiver has drained", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)
		s.mustOb(t, dir, "backup", "enable", "postgres")
		s.rotateWAL(t)
		s.drainArchiver(t)

		s.mustOb(t, dir, "backup", "enable", "postgres")

		before, _ := s.archiverCounts(t)
		s.rotateWAL(t)
		deadline := time.Now().Add(60 * time.Second)
		for {
			archived, failed := s.archiverCounts(t)
			if archived > before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("draining first did not help: %d archived, %d failed, last failed %q",
					archived, failed, s.psql(t, "select last_failed_wal from pg_stat_archiver"))
			}
			time.Sleep(2 * time.Second)
		}
		s.teardown(t, dir)
	})

	// A new database against a repository that still holds the last one's
	// history.
	//
	// `ob destroy --volumes` removes the cluster; the repository is off-host
	// and survives on purpose. Deploying the same application again creates a
	// different PostgreSQL cluster that starts numbering its write-ahead log
	// from 000000010000000000000001 — the same object names the previous
	// cluster already wrote, with different bytes. The repository generation must
	// therefore carry the PostgreSQL system identifier: it changes with the data
	// volume while the application and service names remain the same.
	t.Run("a redeployed database does not collide with the old history", func(t *testing.T) {
		first := s.project(t, endpoint, "v1")
		s.deploy(t, first)
		s.mustOb(t, first, "backup", "enable", "postgres")
		firstID := s.psql(t, "select system_identifier from pg_control_system()")
		s.rotateWAL(t)
		s.teardown(t, first)

		second := s.project(t, endpoint, "v1")
		s.deploy(t, second)
		s.mustOb(t, second, "backup", "enable", "postgres")
		secondID := s.psql(t, "select system_identifier from pg_control_system()")
		if firstID == secondID {
			t.Fatalf("replacement database kept system identifier %s", firstID)
		}
		status := s.mustOb(t, second, "backup", "status", "postgres")
		if !strings.Contains(status, firstID) || !strings.Contains(status, secondID) {
			t.Fatalf("repository generations are not discoverable after local state loss:\n%s", status)
		}
		s.mustOb(t, second, "backup", "drill", "postgres", "--generation", firstID)

		before, _ := s.archiverCounts(t)
		s.rotateWAL(t)
		deadline := time.Now().Add(60 * time.Second)
		for {
			archived, failed := s.archiverCounts(t)
			if archived > before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the new cluster cannot archive over the old history: %d archived, %d failed, last failed %q",
					archived, failed, s.psql(t, "select last_failed_wal from pg_stat_archiver"))
			}
			time.Sleep(2 * time.Second)
		}
		s.teardown(t, second)
	})

	// `destroy` is what an operator runs to get the machine back. Containers
	// are the visible part; the host state ob installed is the part that gets
	// left behind, and a machine that cannot be re-used is a machine that was
	// not destroyed.
	t.Run("destroy leaves no host state", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)
		s.mustOb(t, dir, "backup", "enable", "postgres")
		s.teardown(t, dir)

		for _, probe := range []struct{ name, command, want string }{
			{"release tree", "ls -d /var/lib/ob/observer 2>/dev/null | wc -l", "0"},
			{"backup timers", "systemctl list-units --type=timer --all --no-pager | grep -c ob-backup || true", "0"},
			{"unit files", "ls /etc/systemd/system/ob-backup-* 2>/dev/null | wc -l", "0"},
			{"containers", "docker ps -aq --filter label=ob.app=observer | wc -l", "0"},
			{"networks", "docker network ls --filter label=ob.app=observer -q | wc -l", "0"},
			{"volumes", "docker volume ls -q | grep -c observer || true", "0"},
		} {
			if got := strings.TrimSpace(s.run(t, probe.command)); got != probe.want {
				t.Errorf("destroy left %s behind: %s (want %s)", probe.name, got, probe.want)
			}
		}
	})

	// Without --volumes the data must survive, because that is the difference
	// between tearing an application down and losing the database.
	t.Run("destroy without volumes keeps the data", func(t *testing.T) {
		dir := s.project(t, endpoint, "v1")
		s.deploy(t, dir)
		s.psql(t, "create table if not exists kept(note text)")
		s.psql(t, "insert into kept values (concat('still', ' ', 'here'))")

		out, err := s.obInput(t, dir, s.obHome(t), "observer\ny\n", "destroy")
		if err != nil {
			t.Fatalf("destroy failed: %v\n%s", err, out)
		}
		volumes := strings.TrimSpace(s.run(t, "docker volume ls -q | grep -c observer || true"))
		if volumes == "0" {
			t.Fatal("destroy without --volumes removed the data volumes")
		}

		s.bootstrapped = map[string]bool{}
		s.deploy(t, dir)
		if note := s.psql(t, "select note from kept limit 1"); note != "still here" {
			t.Fatalf("the redeployed service does not hold the row: %q", note)
		}
		s.teardown(t, dir)
	})
}

// teardown removes the application and everything it owns, so the next probe
// starts from a machine that has only what a fresh one would.
func (s *server) teardown(t *testing.T, dir string) {
	t.Helper()
	// Kept deliberately when asked. A probe that fails is a probe whose
	// machine is worth looking at, and tearing it down is how the evidence for
	// the last three wrong theories disappeared before it could be read.
	if os.Getenv("ONEBOX_E2E_KEEP") == "1" {
		t.Log("ONEBOX_E2E_KEEP=1: leaving the application in place")
		return
	}
	if out, err := s.obInput(t, dir, s.obHome(t), "observer\ny\n", "destroy", "--volumes"); err != nil {
		t.Fatalf("teardown failed: %v\n%s", err, out)
	}
	s.bootstrapped = map[string]bool{}
}

// drainArchiver waits until PostgreSQL has shipped everything it is holding.
//
// A segment is pending while a .ready marker exists beside it in
// archive_status. Waiting for that set to empty is the only way to know the
// archiver and a base backup are not about to write the same object name.
func (s *server) drainArchiver(t *testing.T) {
	t.Helper()
	container := strings.TrimSpace(s.run(t,
		`docker ps -q --filter label=com.docker.compose.service=postgres | head -1`))
	deadline := time.Now().Add(60 * time.Second)
	for {
		pending := strings.TrimSpace(s.run(t, `docker exec `+container+
			` sh -c 'ls /var/lib/postgresql/data/pgdata/pg_wal/archive_status/*.ready 2>/dev/null | wc -l'`))
		if pending == "0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the archiver never drained: %s segment(s) still pending", pending)
		}
		time.Sleep(2 * time.Second)
	}
}
