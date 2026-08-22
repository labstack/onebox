package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// Executable backup for the postgres driver.
//
// Everything above this file describes backup: the project declares intent,
// the catalogue declares what each driver could support, the lifecycle state
// records whether it was ever established, and the artifact set records what a
// protected service should look like. None of it runs anything. This file and
// its _ops sibling are the part that does, and they are deliberately narrow —
// one driver, one recovery kind, physical base plus WAL.
//
// Whether a service *is* protected is not decided here. It is durable state on
// the target, observed at project load and bound into the rendered project
// before anything renders, so a policy that was declared but never enabled
// produces an ordinary server rather than one archiving to a repository nobody
// initialised.

// StageBackupRuntime places the verified wal-g binary and its generated
// wrapper on the target, then makes them readable by the service.
//
// The binary is fetched here — on the machine running `ob` — and uploaded,
// rather than downloaded by the target. That keeps the agentless model intact
// and, more importantly, keeps verification on this side of the trust boundary:
// the checksum is pinned in the Onebox binary, so a host with no outbound
// internet still gets backup, and a compromised release page cannot
// substitute a binary that a target-side `curl | sha256sum` would happily
// accept against a checksum from the same source.
func (e *Engine) StageBackupRuntime(ctx context.Context, service string, wrapper []byte) error {
	machine, err := e.targetMachine(ctx)
	if err != nil {
		return err
	}
	asset, expected, err := app.WalgAssetFor(machine)
	if err != nil {
		return err
	}
	n := e.names()
	destination := n.BackupBinaryFile(service)

	present, err := e.fileHasChecksum(ctx, destination, expected)
	if err != nil {
		return err
	}
	if !present {
		st := e.ui.Step("backup runtime wal-g "+app.WalgVersion+" ("+machine+")", false)
		staged, cleanup, err := fetchVerifiedBinary(ctx, app.WalgDownloadURL(asset), expected)
		if err != nil {
			st(err)
			return err
		}
		defer cleanup()
		if err := e.uploadBackupBinary(ctx, n.BackupRuntimeDir(service), staged, destination); err != nil {
			st(err)
			return err
		}
		st(nil)
	}

	// The wrapper is passed in rather than rendered here, because enablement
	// has to stage the runtime *before* it records that the service is
	// protected — and until that record exists there is no bound state to
	// render from. It is rewritten every time: it is derived from the declared
	// credential entry names, which can change without the binary changing.
	wrapperPath := n.BackupWrapperFile(service)
	if err := e.writeServiceFile(ctx, wrapperPath, wrapper); err != nil {
		return fmt.Errorf("cannot place backup wrapper %s: %w", wrapperPath, err)
	}
	// Readable and executable by the unprivileged server user inside the
	// container, which is the whole point of it being there. Safe because it
	// holds no credential — only the names of entries it reads from the
	// environment.
	if err := e.chmodPath(ctx, wrapperPath, "0755"); err != nil {
		return err
	}
	if err := e.stageTrustStore(ctx, service); err != nil {
		return err
	}
	return e.chmodPath(ctx, n.BackupRuntimeDir(service), "0755")
}

// trustStoreCandidates are the certificate authority bundles a Linux host is
// likely to keep, most common first. Debian and Ubuntu — what the qualified
// PostgreSQL images are built from — use the first.
var trustStoreCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// stageTrustStore copies the host's certificate authorities in beside the
// binary, because wal-g runs in the driver's image and that image has none.
//
// `postgres:18` ships two entries under /etc/ssl/certs and no bundle among
// them, so every upload to the HTTPS endpoint an s3-compatible target is
// required to declare fails verification. It failed *late*: the base backup
// completed first, so the error arrived a quarter of an hour in, against a
// server whose archiving was already on.
//
// A host with no bundle is refused here rather than discovered there. The
// alternative is staging nothing, letting the wrapper fall back to the image's
// empty store, and reproducing exactly the failure this exists to prevent —
// only later, and with the database already archiving.
func (e *Engine) stageTrustStore(ctx context.Context, service string) error {
	destination := e.names().BackupTrustStoreFile(service)
	// Copied on the target rather than uploaded from here: the bundle that
	// matters is the one the operator's machine trusts, and this machine may
	// not be the same operating system.
	//
	// Written to a temporary name and renamed, like every other generated file.
	// Safe for a bind mount because what is mounted is the directory, so the
	// replaced file's new inode is still found through it.
	var probe strings.Builder
	probe.WriteString("set -e\n")
	for _, candidate := range trustStoreCandidates {
		probe.WriteString("if [ -r " + q(candidate) + " ]; then\n")
		probe.WriteString("  cp " + q(candidate) + " " + q(destination+".tmp") + "\n")
		probe.WriteString("  chmod 0644 " + q(destination+".tmp") + "\n")
		probe.WriteString("  mv " + q(destination+".tmp") + " " + q(destination) + "\n")
		probe.WriteString("  echo " + q(candidate) + "\n")
		probe.WriteString("  exit 0\n")
		probe.WriteString("fi\n")
	}
	probe.WriteString("exit 1\n")

	res, err := e.T.Run(ctx, probe.String())
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf(
			"service %s: the target holds no certificate authority bundle at any of %s, "+
				"so wal-g cannot verify the backup endpoint from inside the container; "+
				"install the host's CA certificates (on Debian and Ubuntu: apt-get install ca-certificates)",
			service, strings.Join(trustStoreCandidates, ", "))
	}
	return nil
}

// QuiesceArchiver waits until PostgreSQL has shipped everything it is holding,
// so the base backup that follows cannot write an object the archiver is about
// to write differently.
//
// Both wal-g and the archive_command upload into the same WAL namespace. When
// `backup-push` lands a segment PostgreSQL has not archived yet, the copy
// PostgreSQL archives afterwards has different bytes and wal-g refuses it —
// "already archived, contents differ". PostgreSQL archives strictly in order,
// so one refused segment stops the chain for good: archived_count never moves
// again, failed_count climbs, and the recovery window quietly stops advancing
// while the command that caused it reported success.
//
// A forced switch first, because the pending set is what matters and a segment
// still being written is never in it. Waiting for the set to empty is then the
// only statement that both writers cannot be about to touch the same name.
//
// A target that cannot drain is refused rather than backed up anyway: the
// alternative is establishing a backup whose chain is already broken, and
// enablement's caller reverts a failure to unprotected, which leaves the
// service as it was found.
func (e *Engine) QuiesceArchiver(ctx context.Context, service string) error {
	n := e.names()
	exec := "docker exec -u postgres " + q(n.ServiceContainer(service)) +
		" psql -U " + q(app.PgSuperuser) + " -d postgres -Atc "

	if res, err := e.T.Run(ctx, exec+q("select pg_switch_wal();")); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("service %s: cannot close the current write-ahead log segment: %s",
			service, strings.TrimSpace(res.Stderr))
	}

	pending := exec + q("select count(*) from pg_ls_dir('pg_wal/archive_status') f where f like '%.ready';")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res, err := e.T.Run(ctx, pending)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("service %s: cannot inspect the PostgreSQL archive queue: %s",
				service, strings.TrimSpace(res.Stderr))
		}
		if strings.TrimSpace(res.Stdout) == "0" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"service %s: the archiver still holds %s write-ahead log segment(s) it has not shipped; "+
					"taking a base backup now would write objects it is about to write differently and stop the chain — "+
					"check `ob backup status %s` and the archive_command before retrying",
				service, strings.TrimSpace(res.Stdout), service)
		}
		e.Opts.Sleep(2 * time.Second)
	}
}

// PostgresSystemIdentifier reads the identity initdb assigned to this data
// directory. It is stable for the life of the cluster and changes when the
// volume is recreated, which is exactly the boundary a WAL repository must
// separate: fresh clusters reuse the same WAL filenames.
func (e *Engine) PostgresSystemIdentifier(ctx context.Context, service string) (string, error) {
	n := e.names()
	command := "docker exec -u postgres " + q(n.ServiceContainer(service)) +
		" psql -U " + q(app.PgSuperuser) + " -d postgres -Atc " +
		q("select system_identifier::text from pg_control_system();")
	res, err := e.T.Run(ctx, command)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("service %s: cannot read the PostgreSQL system identifier: %s",
			service, strings.TrimSpace(res.Stderr))
	}
	identifier := strings.TrimSpace(res.Stdout)
	if _, err := strconv.ParseUint(identifier, 10, 64); err != nil || identifier == "" {
		return "", fmt.Errorf("service %s: PostgreSQL returned an invalid system identifier %q", service, identifier)
	}
	return identifier, nil
}

// ValidateProtectedDatabaseIdentities proves that each protected PostgreSQL
// volume is the cluster recorded in lifecycle state. It deliberately reads the
// volume without relying on the service container: Compose may be about to
// recreate that container, and a missing volume must be detected before
// Compose silently creates an empty one.
func (e *Engine) ValidateProtectedDatabaseIdentities(ctx context.Context) error {
	n := e.names()
	for _, service := range e.Spec.ServiceNames() {
		if !e.Spec.ServiceIsProtected(service) {
			continue
		}
		state, ok := e.Spec.ServiceRuntimeState(service)
		if !ok {
			continue
		}
		recorded := state.DatabaseSystemIdentifier
		if recorded == "" {
			return fmt.Errorf("service %s is protected by a legacy repository binding with no PostgreSQL system identifier; run `ob backup enable %s` before applying services", service, service)
		}
		declared := e.Spec.Services[service]
		driver := declared.Driver
		if driver == "" {
			driver = service
		}
		if driver != "postgres" {
			continue
		}
		volume := n.ServiceVolume(service, app.DataVolumeFor(declared))
		inspect, err := e.T.Run(ctx, "docker volume inspect "+q(volume))
		if err != nil {
			return err
		}
		if inspect.ExitCode != 0 {
			return fmt.Errorf("service %s is recorded as PostgreSQL cluster %s, but data volume %s is missing; restore that generation or disable backup before applying services", service, recorded, volume)
		}
		image, err := e.Spec.ServiceImageForRuntime(service)
		if err != nil {
			return err
		}
		command := "docker run --rm --entrypoint pg_controldata -v " + q(volume+":/var/lib/postgresql/data:ro") +
			" " + q(image.Image) + " " + q(app.PgDataPath)
		res, err := e.T.Run(ctx, command)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("service %s: cannot verify the PostgreSQL identity in volume %s: %s", service, volume, strings.TrimSpace(res.Stderr))
		}
		actual := postgresControlSystemIdentifier(res.Stdout)
		if actual == "" {
			return fmt.Errorf("service %s: pg_controldata did not report a PostgreSQL system identifier for volume %s", service, volume)
		}
		if actual != recorded {
			return fmt.Errorf("service %s data volume belongs to PostgreSQL cluster %s, but backup lifecycle state is bound to cluster %s; run `ob backup enable %s` to establish a new repository generation before applying services", service, actual, recorded, service)
		}
	}
	return nil
}

func postgresControlSystemIdentifier(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "Database system identifier" {
			identifier := strings.TrimSpace(value)
			if databaseSystemIdentifier.MatchString(identifier) {
				return identifier
			}
		}
	}
	return ""
}

var databaseSystemIdentifier = regexp.MustCompile(`^[0-9]{1,20}$`)

func (e *Engine) targetMachine(ctx context.Context) (string, error) {
	res, err := e.T.Run(ctx, "uname -m")
	if err != nil {
		return "", err
	}
	machine := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || machine == "" {
		return "", fmt.Errorf("cannot determine the target's machine architecture")
	}
	return machine, nil
}

// fileHasChecksum reports whether the target already holds exactly the expected
// bytes. Re-uploading 60MB on every enable would be the kind of cost that makes
// people avoid running the command.
func (e *Engine) fileHasChecksum(ctx context.Context, remotePath, expected string) (bool, error) {
	res, err := e.T.Run(ctx, "sha256sum "+q(remotePath)+" 2>/dev/null | cut -d' ' -f1")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == expected, nil
}

// fetchVerifiedBinary downloads an asset and refuses it unless it hashes to the
// pinned value. The file is never made executable and never leaves the
// temporary directory until it has matched.
func fetchVerifiedBinary(ctx context.Context, url, expected string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ob-backup-runtime-")
	if err != nil {
		return "", nil, fmt.Errorf("create staging directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cleanup()
		return "", nil, fmt.Errorf("fetch %s: %s", url, response.Status)
	}

	staged := filepath.Join(dir, "wal-g")
	file, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, digest), response.Body); err != nil {
		file.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	if observed := hex.EncodeToString(digest.Sum(nil)); observed != expected {
		cleanup()
		return "", nil, fmt.Errorf(
			"the wal-g download does not match its pinned checksum (expected %s, got %s); refusing to place it on the host",
			expected, observed)
	}
	return staged, cleanup, nil
}

// uploadBackupBinary moves the verified binary into place. Upload writes a
// directory, so the staged file is placed alone in one and moved across.
func (e *Engine) uploadBackupBinary(ctx context.Context, runtimeDir, staged, destination string) error {
	res, err := e.T.Run(ctx, "mkdir -p "+q(runtimeDir))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the backup runtime directory: %s", strings.TrimSpace(res.Stderr))
	}
	remoteStaging := runtimeDir + "/.staging"
	if err := e.T.Upload(ctx, filepath.Dir(staged), remoteStaging); err != nil {
		return fmt.Errorf("upload the wal-g binary: %w", err)
	}
	install := "mv -f " + q(remoteStaging+"/"+filepath.Base(staged)) + " " + q(destination) +
		" && chmod 0755 " + q(destination) +
		" && rm -rf " + q(remoteStaging)
	res, err = e.T.Run(ctx, install)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot install the wal-g binary: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (e *Engine) chmodPath(ctx context.Context, target, mode string) error {
	res, err := e.T.Run(ctx, "chmod "+mode+" "+q(target))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot set mode %s on %s", mode, target)
	}
	return nil
}

// WriteBackupLifecycleState places the already-sealed lifecycle record that
// makes a service protected. The record's schema, transitions, and digest all
// belong to the layer above; the engine only puts the bytes on the target,
// under the same fence as every other generated file.
func (e *Engine) WriteBackupLifecycleState(ctx context.Context, service string, body []byte) error {
	n := e.names()
	res, err := e.T.Run(ctx, "mkdir -p "+q(path.Join(n.AppDir(), "backup", "state")))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the backup state directory: %s", strings.TrimSpace(res.Stderr))
	}
	return e.writeServiceFile(ctx, n.BackupLifecycleStateFile(service), body)
}

// ReadBackupLifecycleState returns the raw lifecycle record for a service,
// or nil when none exists. Decoding belongs to the layer that owns the schema;
// the engine only fetches the bytes.
func (e *Engine) ReadBackupLifecycleState(ctx context.Context, service string) ([]byte, error) {
	res, err := e.T.Run(ctx, "cat "+q(e.names().BackupLifecycleStateFile(service))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	if len(res.Stdout) == 0 {
		return nil, nil
	}
	return []byte(res.Stdout), nil
}

// RebindServiceRuntimeStates re-derives the rendered project from lifecycle
// state the caller has just written. Enablement is the one flow that needs it:
// the project was loaded before the service was protected, so without this the
// same run would render the unprotected server it started from.
func (e *Engine) RebindServiceRuntimeStates(states map[string]app.ServiceRuntimeState) error {
	bound, err := e.Spec.WithServiceRuntimeStates(states)
	if err != nil {
		return err
	}
	e.Spec = bound
	return nil
}

// ResolveProtectedImage pins the service image by the digest the host actually
// has, after pulling it.
//
// It is the stock PostgreSQL image — wal-g is mounted in beside it rather than
// baked into a derived one — but it is still pinned, because the reason
// protected image selection is durable state has nothing to do with which image
// it is: the bytes running over a live data directory must not change because a
// tag moved.
// recordedPin and recordedReference come from the service's lifecycle record:
// the digest it was last bound with, and the reference that produced it. When
// the project still declares that same reference and the host still holds those
// bytes, they are what the service keeps running. Re-resolving a tag here would
// mean a re-enable — after a policy edit, or after a disable somebody changed
// their mind about — could move the bytes running over a live data directory
// because the tag moved in the meantime, which is the exact thing pinning
// exists to prevent. It also made re-enable need the registry on a host that
// already had everything it needed, so a rate-limited Docker Hub failed a
// command that had nothing to fetch.
//
// Moving a protected service to a new image is a deliberate act with its own
// path; it is not a side effect of re-running enable.
func (e *Engine) ResolveProtectedImage(ctx context.Context, service, recordedPin, recordedReference string) (string, error) {
	reference, err := e.Spec.ServiceImageForRuntime(service)
	if err != nil {
		return "", err
	}
	declared, err := e.Spec.DeclaredServiceImage(service)
	if err != nil {
		return "", err
	}
	st := e.ui.Step("protected image "+reference.Image, false)
	if recordedPin != "" && recordedReference == declared && containsDigest(recordedPin) {
		held, err := e.imagePresentByDigest(ctx, recordedPin)
		if err != nil {
			st(err)
			return "", err
		}
		if held {
			st(nil)
			return recordedPin, nil
		}
	}
	// A digest already on the host is not pulled again. The reference is
	// immutable by construction, so a second pull cannot return different bytes
	// — it only spends registry quota and turns a re-enable into a failure on a
	// host that is offline or rate-limited while holding exactly what it needs.
	// A tag still resolves through the registry, because a tag can move.
	present, err := e.imagePresentByDigest(ctx, reference.Image)
	if err != nil {
		st(err)
		return "", err
	}
	if present {
		st(nil)
		return reference.Image, nil
	}
	res, err := e.T.Run(ctx, "docker pull "+q(reference.Image))
	if err != nil {
		st(err)
		return "", err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cannot pull %s: %s", reference.Image, lastLines(res.Stderr, 3))
		st(err)
		return "", err
	}
	res, err = e.T.Run(ctx, "docker image inspect --format '{{index .RepoDigests 0}}' "+q(reference.Image))
	if err != nil {
		st(err)
		return "", err
	}
	pinned := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || !containsDigest(pinned) {
		err := fmt.Errorf(
			"%s has no registry digest on this host; a protected service runs an image pinned by digest, so it must come from a registry rather than a local build",
			reference.Image)
		st(err)
		return "", err
	}
	st(nil)
	return pinned, nil
}

// RemoveBackupCredentials deletes the target-side credential file for a
// service that is no longer protected. The repository it pointed at is left
// exactly as it is.
func (e *Engine) RemoveBackupCredentials(ctx context.Context, service string, last *app.BackupEffectiveProjection) error {
	if last == nil {
		return nil
	}
	path := e.names().BackupCredentialFile(service, last.Policy.Target)
	res, err := e.T.Run(ctx, "rm -f "+q(path))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot remove the backup credential file %s", path)
	}
	return nil
}

// ReportDisabled says what disablement did and, more usefully, what it did not.
//
// The second line used to promise that `ob backup status` and `ob backup
// restore` still worked. They do not: both run wal-g inside the service
// container, and an unprotected service mounts neither the binary nor the
// credentials. The backups themselves are untouched, which is the part that
// matters, and the way back to them is to enable backup again.
func (e *Engine) ReportDisabled(service string) {
	e.ui.Successf("%s is no longer archiving; its schedules are removed", service)
	e.ui.Infof("every backup already taken is untouched in the repository. Reading or recovering from them "+
		"needs backup enabled again (`ob backup enable %s`), because the tooling and credentials that "+
		"reach the repository live in the protected service", service)
}

// ReportTargetMoved says that this enablement bound the service to a different
// repository from the one it was archiving to, which is a fact with a
// consequence: the new repository starts at the backup this run is about to
// take, so the declared recovery window begins now.
func (e *Engine) ReportTargetMoved(service, from, to string) {
	e.ui.Infof("%s now archives to backup target %q. What %q holds is untouched, but this repository "+
		"starts from the backup being taken now, so the declared recovery window begins here",
		service, to, from)
}

// VerifyBackupRuntime asks the target whether what is staged there is still
// what Onebox expects.
//
// This is the drift question that matters, and it is asked of the target
// directly: the binary that pushes the backups must be the one whose checksum
// is pinned in this release, and the wrapper that gives it credentials must be
// the one the project renders. A descriptor written beside them saying what they
// ought to be would only be somewhere for the two to disagree.
func (e *Engine) VerifyBackupRuntime(ctx context.Context, service string) ([]string, error) {
	n := e.names()
	var issues []string

	machine, err := e.targetMachine(ctx)
	if err != nil {
		return nil, err
	}
	_, expected, err := app.WalgAssetFor(machine)
	if err != nil {
		return nil, err
	}
	matches, err := e.fileHasChecksum(ctx, n.BackupBinaryFile(service), expected)
	if err != nil {
		return nil, err
	}
	if !matches {
		issues = append(issues, fmt.Sprintf(
			"the wal-g binary at %s is not the %s build this release pins; re-run `ob service apply` to replace it",
			n.BackupBinaryFile(service), app.WalgVersion))
	}

	wrappers, err := e.Spec.RenderServiceBackupWrappers(e.Opts.Environment)
	if err != nil {
		return nil, err
	}
	wanted, ok := wrappers[n.BackupWrapperFile(service)]
	if !ok {
		return issues, nil
	}
	res, err := e.T.Run(ctx, "cat "+q(n.BackupWrapperFile(service))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	if res.Stdout != string(wanted) {
		issues = append(issues, fmt.Sprintf(
			"the credential wrapper at %s is not what this project renders; re-run `ob service apply` to replace it",
			n.BackupWrapperFile(service)))
	}
	archiving, err := e.archivingIssues(ctx, service)
	if err != nil {
		return nil, err
	}
	return append(issues, archiving...), nil
}

// archivingIssues asks the running server whether it is still archiving, and
// whether it is doing so often enough for the loss the policy tolerates.
//
// The binary and the wrapper being correct says the tooling is in place. It
// says nothing about whether the server is using it: `archive_mode` can be
// turned off, `archive_command` can be replaced, and `archive_timeout` can be
// raised past the declared maximum data loss — each of which leaves every
// existing backup intact and quietly stops the recovery point advancing the way
// the policy promises. That is a state worth naming, because nothing else here
// notices it.
func (e *Engine) archivingIssues(ctx context.Context, service string) ([]string, error) {
	projection, err := e.Spec.EffectiveBackupProjection(service)
	if err != nil {
		return nil, err
	}
	n := e.names()
	// Connected as the role the driver creates, against the database that always
	// exists.
	//
	// Two earlier spellings of this line both failed and were both read by the
	// exit-code branch below as "the server is down", so the check silently did
	// nothing on a perfectly healthy server: asking as the OS user reaches a
	// role that does not exist, and asking as the right role without a database
	// reaches one named after the role, which does not exist either. Only
	// running it against a live server showed that — the unit tests were happy
	// with a command that never worked.
	read := "docker exec -u postgres " + q(n.ServiceContainer(service)) +
		" psql -U " + q(app.PgSuperuser) + " -d postgres -Atc " + q("show archive_mode;") +
		" -Atc " + q("show archive_command;") +
		" -Atc " + q("show archive_timeout;")
	res, err := e.T.Run(ctx, read)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		// Not an assertion that archiving is broken: the server may simply be
		// down, which every other part of status already reports.
		return nil, nil
	}
	fields := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(fields) < 3 {
		return nil, nil
	}
	mode, command, timeout := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
	var issues []string
	if mode != "on" {
		issues = append(issues, fmt.Sprintf(
			"the server has archive_mode %q, so no write-ahead log is reaching the repository and the recovery point stopped advancing; re-run `ob backup enable %s`", mode, service))
	}
	if !strings.Contains(command, app.WalgBinary) {
		issues = append(issues, fmt.Sprintf(
			"the server's archive_command is not the one Onebox installed, so where the write-ahead log goes is not what this project describes; re-run `ob backup enable %s`", service))
	}
	if declared, ok := app.ParseDuration(projection.Policy.MaxDataLoss); ok {
		if observed, parsed := app.ParsePostgresDuration(timeout); parsed && observed > declared {
			issues = append(issues, fmt.Sprintf(
				"the server closes a write-ahead log segment every %s, but the policy tolerates losing at most %s; an idle database can lose more than the policy permits",
				timeout, projection.Policy.MaxDataLoss))
		}
	}
	return issues, nil
}

// imagePresentByDigest reports whether a digest-pinned reference is already on
// the host. A tag always answers false: it may point somewhere else now, which
// is the reason backup pins in the first place.
func (e *Engine) imagePresentByDigest(ctx context.Context, reference string) (bool, error) {
	if !containsDigest(reference) {
		return false, nil
	}
	res, err := e.T.Run(ctx, "docker image inspect "+q(reference)+" >/dev/null 2>&1 && echo present || echo absent")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == "present", nil
}

// containsDigest reports whether a reference names exact bytes rather than a
// tag. It is the one place that decides, so the pull-skipping guard and the
// pinning check cannot disagree about what "pinned" means.
func containsDigest(reference string) bool { return strings.Contains(reference, "@sha256:") }
