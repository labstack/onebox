package app

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// wal-g is the backup engine for the postgres driver: a physical base
// backup plus continuous WAL archiving, which is the only combination that can
// honour the point-in-time recovery the policy declares. A logical dump cannot,
// and a copy of a running data directory is the generic live-volume archive the
// contract refuses outright.
//
// It runs from a verified binary staged on the host and mounted into the stock
// PostgreSQL image, rather than from a PostgreSQL image Onebox builds and
// publishes. That is the whole reason this file is short. wal-g links against
// libc and nothing else, and takes its entire configuration from the
// environment — so there is no image to maintain, no configuration file to
// place, and no second copy of anything to keep in step with the project.
//
// pgBackRest was implemented first and replaced. It is a fine tool, but it
// needs 41 shared libraries, so it cannot be dropped into the official image
// and forces a derived one; and it is configured by a file, which brought the
// file's own problems — an atomically replaced config vanishing from a running
// container, credential names colliding with its option namespace, and a
// restore_command it writes as the absolute path of its own binary.

// PgDataPath is the data directory the postgres driver runs with. Every wal-g
// command that touches the cluster needs it exactly: the driver sets PGDATA to
// this subdirectory so the volume can hold a lost+found without confusing
// initdb, and pointing a backup at the volume root captures the wrong tree.
const PgDataPath = "/var/lib/postgresql/data/pgdata"

// WalgMountPath is where the staged binary and its wrapper are mounted inside
// the container, read-only. Outside /usr/local/bin deliberately: the mount must
// not shadow anything the official image ships.
const WalgMountPath = "/opt/onebox/backup"

// WalgBinary is the wrapper Onebox stages beside wal-g, and what every caller
// invokes. It exists because wal-g reads its credentials from fixed AWS_* names
// while a backup target names its own entries, so something has to bridge the
// two — and doing it here keeps the project's vocabulary out of wal-g's and
// wal-g's out of the operator's encrypted file.
const WalgBinary = WalgMountPath + "/ob-wal-g"

// WalgTrustStore is the host trust store as the container sees it. The staged
// copy lands beside the binary, inside the directory already mounted read-only
// at WalgMountPath.
const WalgTrustStore = WalgMountPath + "/ca-certificates.crt"

// WalgRepositoryKeyEntry is the credential entry holding the repository
// encryption key. Unlike the destination keys it has a fixed name: the key is
// Onebox's own requirement rather than a property of the destination, so there
// is no backup_targets field to indirect through.
const WalgRepositoryKeyEntry = "OB_REPOSITORY_KEY"

// WalgPrefix is the repository location for one protected database generation.
//
// The application and service are joined with the injective rule the rest of
// the derived names use, not a hyphen. Hyphens are legal in both, so `a-b`/`c`
// and `a`/`b-c` must not land on the same prefix. A PostgreSQL system identifier
// then separates successive clusters for the same service. Every fresh cluster
// starts WAL numbering at the same name, so sharing that namespace would make
// the first new segment collide with the old cluster's history.
//
// An empty generation names the legacy layout. It remains readable so a cluster
// protected before generations existed can keep its established history.
func WalgPrefix(target BackupTarget, app, service, generation string) string {
	segments := []string{strings.Trim(target.Prefix, "/"), Join(app, service)}
	if segments[0] == "" {
		segments = segments[1:]
	}
	if generation != "" {
		segments = append(segments, "clusters", generation)
	}
	return "s3://" + target.Bucket + "/" + strings.Join(segments, "/")
}

// WalgArchiveCommand is what the server runs for each completed WAL segment.
// A failure here must fail the archive: postgres then retries, and WAL
// accumulates on the data volume until it succeeds. That is what keeps the
// recovery window continuous instead of quietly gapped, and it is why archiving
// is worth alerting on — an unreachable repository eventually fills the volume
// rather than losing history silently.
func WalgArchiveCommand() string { return WalgBinary + " wal-push %p" }

// WalgEnvironment is the non-secret configuration a protected service runs
// with. Every value here is derived from the project and safe to read: the
// destination's location, not its keys.
func WalgEnvironment(target BackupTarget, repository, database, service string) (map[string]any, error) {
	if target.Kind != "s3-compatible" {
		return nil, fmt.Errorf("backup for %q: unsupported target kind %q", service, target.Kind)
	}
	endpoint, err := url.Parse(target.Endpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("backup for %q: endpoint %q is not a URL naming a host", service, target.Endpoint)
	}
	if endpoint.Scheme == "https" && target.TLS == "skip-verify" {
		// wal-g offers no way to skip certificate verification: its only
		// transport controls are the endpoint protocol and a CA file. Accepting
		// this would mean verifying anyway while the project says otherwise.
		return nil, errf("recovery_objective_unsupported", "backup_targets.tls", "ob validate",
			"backup for %q cannot skip certificate verification on an https endpoint: wal-g has no such option. "+
				"Install the certificate authority on the host so the certificate verifies, or use an http endpoint if the destination is on a trusted network",
			service)
	}
	env := map[string]any{
		"WALG_S3_PREFIX": repository,
		"AWS_ENDPOINT":   target.Endpoint,
		// Path-style addressing, because an S3-compatible endpoint is usually
		// not the one provider whose virtual-host naming works everywhere. A
		// bucket name is then a path segment rather than a subdomain that has
		// to resolve.
		"AWS_S3_FORCE_PATH_STYLE": "true",
		// The repository is encrypted whatever the destination does, because
		// the schema refuses a policy whose recovery kind has no declared
		// encryption mode. The key is hex so it can be a printable line in the
		// operator's encrypted file rather than 32 raw bytes.
		"WALG_LIBSODIUM_KEY_TRANSFORM": "hex",
		// A completed segment is archived when it fills or when the server
		// switches; refusing to overwrite one already in the repository turns a
		// duplicated segment into a loud failure instead of a rewritten history.
		"WALG_PREVENT_WAL_OVERWRITE": "true",
		// backup-push connects to the local server over its Unix socket, so it
		// needs no password and opens no TCP port to do it.
		//
		// PGDATABASE is not optional: left unset libpq defaults the database to
		// the user name, and the driver's user is `onebox` while its database
		// is the application's — so every command fails with `database "onebox"
		// does not exist`, which reads like a broken cluster rather than a
		// missing variable.
		"PGHOST":     "/var/run/postgresql",
		"PGUSER":     PgSuperuser,
		"PGDATABASE": database,
		"PGDATA":     PgDataPath,
	}
	if target.Region != "" {
		env["AWS_REGION"] = target.Region
	} else {
		// wal-g requires a region even where the destination ignores it.
		// Written down rather than left implicit.
		env["AWS_REGION"] = "us-east-1"
	}
	// The transport comes from the endpoint's own scheme, which is the only
	// unambiguous source: `tls` says what the operator will accept, not what
	// the endpoint speaks. Mapping skip-verify to plaintext — as this first did
	// — sent credentials in the clear to an https endpoint that had merely
	// presented a self-signed certificate.
	env["S3_ENDPOINT_PROTOCOL"] = endpoint.Scheme
	return env, nil
}

// PgSuperuser is the role the postgres driver creates. It must agree with the
// driver's `user` field, and the contract test holds the two together. wal-g
// connects as it rather than as the operating-system user, which is `postgres`
// — a role the driver never creates, because Onebox owns the identity and makes
// it the application's so two projects on one host cannot silently share one.
const PgSuperuser = "onebox"

// WalgCredentialEntries are the entry names the target-side credential file
// must define: the two or three the project named for the destination, plus the
// fixed repository key. Names only — no value is produced, read, or returned.
func WalgCredentialEntries(target BackupTarget) []string {
	entries := []string{
		target.Credentials.AccessKeyEntry,
		target.Credentials.SecretKeyEntry,
		WalgRepositoryKeyEntry,
	}
	if target.Credentials.SessionTokenEntry != "" {
		entries = append(entries, target.Credentials.SessionTokenEntry)
	}
	sort.Strings(entries)
	return entries
}

// RenderWalgWrapper generates the wrapper staged beside the binary.
//
// It maps the entry names the project declared onto the fixed AWS_* names wal-g
// reads, and it is generated rather than shipped because those names come from
// the project. It holds no value: the wrapper reads them from the environment,
// which the container gets from the mode-0600 credential file on the host.
func RenderWalgWrapper(target BackupTarget) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generated by Onebox. Maps the credential entry names this project\n")
	b.WriteString("# declared onto the fixed names wal-g reads, then runs it.\n")
	b.WriteString("#\n")
	b.WriteString("# No credential is in this file. The values arrive in the environment\n")
	b.WriteString("# from the mode-0600 credential file on the host; only the names are\n")
	b.WriteString("# here, and the names are not secret.\n")
	b.WriteString("set -eu\n")
	assign := func(walgName, entry string) {
		if entry == "" {
			return
		}
		b.WriteString("if [ -n \"${" + entry + "-}\" ]; then\n")
		b.WriteString("    " + walgName + "=\"$" + entry + "\"\n")
		b.WriteString("    export " + walgName + "\n")
		b.WriteString("fi\n")
	}
	assign("AWS_ACCESS_KEY_ID", target.Credentials.AccessKeyEntry)
	assign("AWS_SECRET_ACCESS_KEY", target.Credentials.SecretKeyEntry)
	assign("AWS_SESSION_TOKEN", target.Credentials.SessionTokenEntry)
	assign("WALG_LIBSODIUM_KEY", WalgRepositoryKeyEntry)
	// The trust store, if one was staged.
	//
	// wal-g executes inside the driver's image, and the official PostgreSQL
	// images ship no certificate authorities, so an HTTPS endpoint — which
	// every s3-compatible target is required to be — fails verification with
	// "certificate signed by unknown authority". Three names because three
	// layers can do the verifying: wal-g's own S3 setting, the AWS SDK
	// underneath it, and Go's crypto/tls under that.
	//
	// Guarded on the file being readable rather than exported unconditionally:
	// a host with no bundle should leave the image's own store in play, and
	// pointing these at a path that does not exist makes the SDK fail with a
	// worse error than the one it replaces.
	b.WriteString("if [ -r " + WalgTrustStore + " ]; then\n")
	for _, name := range []string{"WALG_S3_CA_CERT_FILE", "AWS_CA_BUNDLE", "SSL_CERT_FILE"} {
		b.WriteString("    " + name + "=" + WalgTrustStore + "\n")
		b.WriteString("    export " + name + "\n")
	}
	b.WriteString("fi\n")
	b.WriteString("exec " + WalgMountPath + "/wal-g \"$@\"\n")
	return []byte(b.String())
}

// WalgVersion is the wal-g release Onebox stages, pinned in the binary rather
// than resolved at run time, together with the checksum of each architecture's
// asset. The checksums are the provenance: the binary is verified against these
// before it is ever placed on a host, so a compromised release page cannot
// substitute one. They were taken from the release and confirmed against the
// binary this was validated with.
const WalgVersion = "v3.0.8"

// walgChecksums maps the target's `uname -m` to the published asset and its
// SHA-256. A host reporting anything else is refused rather than guessed at.
var walgChecksums = map[string]struct{ Asset, SHA256 string }{
	"x86_64": {
		Asset:  "wal-g-pg-22.04-amd64",
		SHA256: "f30544c5ce93cf83b87578e3c4a2e9c0e0ffc3d160ef89ecddaf75f397d98deb",
	},
	"aarch64": {
		Asset:  "wal-g-pg-22.04-aarch64",
		SHA256: "794d1a81f0c27825a1603bd39c0f2cf5dd8bed7cc36b598ca05d8d963c3d5fcf",
	},
}

// WalgAssetFor returns the download name and expected checksum for a target's
// machine architecture.
//
// The assets are built against Ubuntu 22.04 and link against glibc, which is
// what the official Debian-based PostgreSQL images provide. An Alpine variant
// would not run them, which is why the driver's image is not a matter of taste.
func WalgAssetFor(machine string) (asset, sha256 string, err error) {
	entry, ok := walgChecksums[normalizeMachine(machine)]
	if !ok {
		return "", "", fmt.Errorf(
			"no verified wal-g build for machine architecture %q; backup supports x86_64 and aarch64", machine)
	}
	return entry.Asset, entry.SHA256, nil
}

// WalgDownloadURL is where the pinned asset comes from. Check is by the
// checksum above, not by trusting this location.
func WalgDownloadURL(asset string) string {
	return "https://github.com/wal-g/wal-g/releases/download/" + WalgVersion + "/" + asset
}

// serviceBackup is everything renderService needs to run a service under
// backup. It is derived from observed durable lifecycle state rather than
// from the presence of a policy, for the same reason image selection is: a
// declared intent is not an established backup, and rendering a server with
// an archive_command pointing at a repository that was never initialised would
// take the database down at its next WAL switch.
type serviceBackup struct {
	RuntimeHostDir string
	CredentialFile string
	ArchiveCommand string
	ArchiveTimeout string
	Environment    map[string]any
}

// backupForRender returns the rendering inputs for a protected service, or
// nil when the service is not running under established backup.
//
// Both live states qualify: `enabled` is the ordinary case, and
// `disable-pending` still archives, because disablement has not completed and
// stopping the archive first would put a gap in the recovery window that the
// retained history claims is continuous.
func (r *Resolved) backupForRender(n Names, serviceName string) (*serviceBackup, error) {
	state, observed := r.serviceRuntime[serviceName]
	if !observed || (state.BackupState != "enabled" && state.BackupState != "disable-pending") {
		return nil, nil
	}
	service, ok := r.Services[serviceName]
	if !ok {
		return nil, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	if driverName != "postgres" {
		// Every other driver's backup is declared in the lifecycle
		// catalogue but has no executable renderer yet. Refusing here is the
		// difference between "not implemented" and a service that runs as if
		// the policy had never been written.
		return nil, errf("backup_driver_unsupported", "services."+serviceName+".backup", "ob validate",
			"driver %q has no executable backup renderer; only postgres is executable today", driverName)
	}
	projection, err := r.renderBackupProjection(serviceName, service, state)
	if err != nil {
		return nil, err
	}
	// The RPO is the reason archive_timeout exists. Without it a quiet database
	// archives only when a 16MB segment fills, so the newest recoverable point
	// can trail by hours whatever the policy says. Forcing a segment switch at
	// the declared interval is what makes max_data_loss a bound rather than
	// an aspiration.
	maximumDataLoss, err := PositiveDuration(projection.Policy.MaxDataLoss)
	if err != nil {
		return nil, err
	}
	repository, err := r.BackupRepository(serviceName)
	if err != nil {
		return nil, err
	}
	environment, err := WalgEnvironment(projection.Target, repository, r.Spec.Name, serviceName)
	if err != nil {
		return nil, err
	}
	environment["OB_S3_KEY_ENTRY"] = projection.Target.Credentials.AccessKeyEntry
	environment["OB_S3_SECRET_ENTRY"] = projection.Target.Credentials.SecretKeyEntry
	return &serviceBackup{
		RuntimeHostDir: n.BackupRuntimeDir(serviceName),
		CredentialFile: n.BackupCredentialFile(serviceName, projection.Policy.Target),
		ArchiveCommand: WalgArchiveCommand(),
		ArchiveTimeout: fmt.Sprintf("%ds", int(maximumDataLoss.Seconds())),
		Environment:    environment,
	}, nil
}

// RenderServiceBackupWrappers generates the credential wrapper for every
// service running under established backup, keyed by the host path it must
// be written to. It is separate from RenderServices because the two are placed
// separately: the wrapper and the binary beside it must be in place before the
// container that mounts them starts.
func (r *Resolved) RenderServiceBackupWrappers(env string) (map[string][]byte, error) {
	n := r.Spec.NamesFor(env)
	out := map[string][]byte{}
	for _, name := range sortedKeys(r.Spec.Services) {
		backup, err := r.backupForRender(n, name)
		if err != nil {
			return nil, err
		}
		if backup == nil {
			continue
		}
		projection, err := r.renderBackupProjection(name, r.Services[name], r.serviceRuntime[name])
		if err != nil {
			return nil, err
		}
		out[n.BackupWrapperFile(name)] = RenderWalgWrapper(projection.Target)
	}
	return out, nil
}

// renderBackupProjection is the policy and target a *running* protected
// service is archiving under.
//
// The recorded projection wins over the project's current intent, and that
// ordering is the point. Enablement writes down exactly what it bound, and the
// server has been archiving to that repository ever since. If someone edits the
// policy — or deletes it — the archive does not retroactively move, so
// rendering from the edited project would point a live server at a repository
// its own history is not in. The project's intent takes effect at the next
// enablement, which is where the change can be made deliberately.
func (r *Resolved) renderBackupProjection(serviceName string, service Service, state ServiceRuntimeState) (BackupEffectiveProjection, error) {
	_ = service
	_ = state
	projection, err := r.EffectiveBackupProjection(serviceName)
	if err != nil {
		return BackupEffectiveProjection{}, errf("backup_state_incomplete", "services."+serviceName+".backup", "ob backup status "+serviceName,
			"service %s is protected but neither its durable state nor the project says what it is protected by; restore the policy or disable backup", serviceName)
	}
	return projection, nil
}

// ServiceIsProtected reports whether a service runs under established
// backup, from durable state rather than from the project's intent.
func (r *Resolved) ServiceIsProtected(serviceName string) bool {
	state, observed := r.serviceRuntime[serviceName]
	return observed && (state.BackupState == "enabled" || state.BackupState == "disable-pending")
}

// BackupRepository returns the exact repository generation recorded for a
// protected service. The empty generation is the pre-generation layout and is
// intentionally retained for compatibility with an established history.
func (r *Resolved) BackupRepository(serviceName string) (string, error) {
	state, observed := r.serviceRuntime[serviceName]
	if !observed || (state.BackupState != "enabled" && state.BackupState != "disable-pending") {
		return "", errf("backup_state_incomplete", "services."+serviceName+".backup", "ob backup status "+serviceName,
			"service %s has no established backup repository", serviceName)
	}
	projection, err := r.EffectiveBackupProjection(serviceName)
	if err != nil {
		return "", err
	}
	return WalgPrefix(projection.Target, r.Spec.Name, serviceName, state.BackupRepositoryGeneration), nil
}

// normalizeMachine folds the spellings of one architecture onto a single name.
//
// `uname -m` is not standardised: Linux says aarch64 where Darwin says arm64,
// and amd64 appears for x86_64. The architecture that matters is the one the
// *container* runs, which is Linux — so a Darwin host reporting arm64 still
// needs the Linux aarch64 build, and folding the names is exactly right rather
// than merely convenient.
func normalizeMachine(machine string) string {
	switch strings.TrimSpace(strings.ToLower(machine)) {
	case "aarch64", "arm64", "armv8l":
		return "aarch64"
	case "x86_64", "amd64", "x64":
		return "x86_64"
	default:
		return strings.TrimSpace(machine)
	}
}

// ValidateWalgCredentials checks decrypted credential material against what the
// repository needs, before any of it reaches the target.
//
// Every problem is reported at once. An operator fixing a SOPS file one error
// message at a time is an operator doing four decrypt-edit-encrypt cycles to
// learn what could have been said in one.
func ValidateWalgCredentials(plaintext []byte, target BackupTarget) error {
	values := map[string]string{}
	for index, line := range strings.Split(string(plaintext), "\n") {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return errf("backup_credentials_invalid", "backup_targets."+target.Credentials.File, "ob backup enable",
				"the decrypted credential file has no name=value on line %d", index+1)
		}
		// Quotes are stripped here for the same reason the installer strips
		// them: a shell-sourced dotenv commonly carries them, and judging the
		// quoted form would reject a perfectly good 64-character key for being
		// 66 characters — with a message about hex that says nothing true.
		values[strings.TrimSpace(name)] = unquoteCredentialValue(value)
	}

	var missing []string
	for _, entry := range WalgCredentialEntries(target) {
		if values[entry] == "" {
			missing = append(missing, entry)
		}
	}
	if len(missing) > 0 {
		return errf("backup_credentials_invalid", "backup_targets."+target.Credentials.File, "ob backup enable",
			"the credential file does not define %s", strings.Join(missing, ", "))
	}

	// The repository key is checked here rather than discovered by wal-g at the
	// first backup. It is read as hex, so a passphrase-shaped value is not a
	// weak key — it is a key wal-g refuses outright, and finding that out from
	// a failed backup is finding it out too late.
	key := values[WalgRepositoryKeyEntry]
	if len(key) != 64 || strings.TrimLeft(strings.ToLower(key), "0123456789abcdef") != "" {
		return errf("backup_credentials_invalid", "backup_targets."+target.Credentials.File, "ob backup enable",
			"%s must be exactly 64 hexadecimal characters — it is a 32-byte key, not a passphrase. Generate one with `openssl rand -hex 32`",
			WalgRepositoryKeyEntry)
	}
	return nil
}

// DataVolumeFor is the durable volume a service's data lives in, as the
// generated runtime names it. Recovery needs it because putting recovered data
// in service means replacing exactly that volume and nothing else.
func DataVolumeFor(s Service) string { return dataVolume(s) }

// WalgRetentionTimeLayout is how wal-g's `delete retain --after` reads a
// timestamp, and WalgRetentionDateFormat is the same shape for `date` on the
// target. They must agree: the scheduled unit computes the cutoff with one and
// wal-g parses it with the other.
const (
	WalgRetentionTimeLayout = "2006-01-02T15:04:05"
	WalgRetentionDateFormat = `%Y-%m-%dT%H:%M:%S`
)

// WalgRetainCount is how many base backups must be kept to honour both
// retention bounds at once.
//
// keep and window are both *minimums*: at least this
// many recoverable bases, and at least this much continuous history. Retention
// must therefore satisfy whichever is larger, and on a frequent schedule that is
// the window — a service backing up every five minutes under a seven-day window
// needs far more than the handful of generations the count alone would keep.
//
// It is computed here rather than at run time because wal-g offers no working
// time bound. Its `--after` flag is documented and ignored, and `delete before
// FIND_FULL <timestamp>` deletes nothing; both were measured against 3.0.8
// rather than taken from the help text. `retain FULL n` does work, and both
// bounds are known from the policy alone, so the arithmetic the policy already
// implies is done up front and expressed as a count.
func WalgRetainCount(policy BackupPolicy) (int, error) {
	window, err := PositiveDuration(policy.Retention.Window)
	if err != nil {
		return 0, err
	}
	dayGap, exact := maximumCronGap(policy.Schedule.Cron)
	perDay, counted := cronRunsPerFiringDay(policy.Schedule.Cron)
	if !exact || dayGap <= 0 || !counted || perDay <= 0 {
		// A schedule whose spacing cannot be bounded cannot say how many backups
		// a window holds. The generation floor is what remains, and it is the
		// operator's declared minimum rather than a guess.
		return policy.Retention.Keep, nil
	}
	// Backups in the window = firing days in the window × runs per firing day.
	// Plus one, because the oldest backup kept must be *older* than the window:
	// recovering to the window's earliest moment replays forward from the base
	// taken before it.
	firingDays := float64(window) / float64(dayGap)
	needed := int(math.Ceil(firingDays*float64(perDay))) + 1
	if needed < policy.Retention.Keep {
		return policy.Retention.Keep, nil
	}
	return needed, nil
}

// cronRunsPerFiringDay counts how many times a schedule fires on a day it fires
// at all, from its minute and hour fields.
//
// maximumCronGap deliberately ignores those fields — it answers a different
// question, the longest gap in *days*, which is what a drill cadence is checked
// against. Using it alone as a backup interval reads "*/5 * * * *" as daily and
// retains two generations for a service that takes 288 a day.
func cronRunsPerFiringDay(expression string) (int, bool) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return 0, false
	}
	minutes, ok := cronFieldCount(fields[0], 60)
	if !ok {
		return 0, false
	}
	hours, ok := cronFieldCount(fields[1], 24)
	if !ok {
		return 0, false
	}
	return minutes * hours, true
}

// cronFieldCount counts the values one cron field selects, over the forms a
// schedule realistically uses: every value, a step, a list, or one value.
func cronFieldCount(field string, size int) (int, bool) {
	if field == "*" {
		return size, true
	}
	if step, ok := strings.CutPrefix(field, "*/"); ok {
		every, err := strconv.Atoi(step)
		if err != nil || every <= 0 || every > size {
			return 0, false
		}
		return (size + every - 1) / every, true
	}
	count := 0
	for _, part := range strings.Split(field, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 0 || value >= size {
			return 0, false
		}
		count++
	}
	if count == 0 {
		return 0, false
	}
	return count, true
}

// unquoteCredentialValue removes one matched pair of surrounding quotes, which
// is what a shell would do when sourcing the file and what the installer does
// when normalising it.
func unquoteCredentialValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}
