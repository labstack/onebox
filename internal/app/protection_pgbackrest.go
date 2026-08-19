package app

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// pgBackRest is the protection engine for the postgres driver: a physical base
// backup plus continuous WAL archiving, which is the only combination that can
// honour the point-in-time recovery the policy declares. A logical dump cannot,
// and a copy of a running data directory is the generic live-volume archive the
// contract refuses outright.
//
// Three things are rendered from a declared policy and nothing else: the
// configuration file, the archive command the server runs, and the names of the
// credential entries the container should read. No secret passes through here.

// PgDataPath is the data directory the postgres driver runs with. pg1-path must
// name it exactly. Pointing at the volume root instead fails at stanza-create
// with "unable to find primary cluster", because the driver sets PGDATA to this
// subdirectory so the volume can hold a lost+found without confusing initdb.
const PgDataPath = "/var/lib/postgresql/data/pgdata"

// PgSuperuser is the role the postgres driver creates. It must agree with the
// driver's `user` field; the contract test holds the two together.
const PgSuperuser = "onebox"

// PgBackRestConfDir is where Onebox's generated configuration is mounted inside
// the container. It is deliberately not /etc/pgbackrest: the image's entrypoint
// symlinks this file into pgBackRest's own configuration directory and writes
// the credentials beside it as an include, and it cannot do that into a
// read-only mount.
const PgBackRestConfDir = "/etc/onebox"

// PgBackRestCipherEntry is the credential entry holding the repository
// passphrase. Unlike the destination keys it has a fixed name: the passphrase
// is Onebox's own requirement rather than a property of the destination, so
// there is no backup_targets field to indirect through.
//
// The OB_ prefix is not decoration. pgBackRest reads every PGBACKREST_<OPTION>
// variable in the environment as an option, so the obvious name for this —
// PGBACKREST_REPO_PASSWORD — is parsed as a `repo-password` option that does
// not exist, and every single command then prints "environment contains invalid
// option 'repo-password'". Naming it outside that namespace is what keeps the
// credential a credential instead of a malformed option.
const PgBackRestCipherEntry = "OB_REPOSITORY_PASSPHRASE"

// PgBackRestStanza is the repository's name for one protected service. It
// carries the application so two projects sharing a bucket cannot collide, and
// it is stable by construction: the stanza names the repository layout, so
// changing it later orphans every backup taken before the change.
func PgBackRestStanza(app, service string) string {
	return app + "-" + service
}

// PgBackRestArchiveCommand is what the server runs for each completed WAL
// segment. A failure here must fail the archive: postgres then retries, and WAL
// accumulates on the data volume until it succeeds. That is the behaviour that
// keeps the recovery window continuous instead of quietly gapped, and it is why
// archiving is worth alerting on — an unreachable repository eventually fills
// the volume rather than losing history silently.
func PgBackRestArchiveCommand(app, service string) string {
	return PgBackRestBinary + " --stanza=" + PgBackRestStanza(app, service) + " archive-push %p"
}

// PgBackRestBinary is what every caller invokes. In the protected image the
// name resolves to a wrapper that puts the repository credentials in scope
// before exec'ing the real binary, which is why it is the bare name rather than
// a path: pgBackRest writes a bare `pgbackrest` into postgresql.auto.conf as
// the restore_command and offers no way to change it, so the bare name is the
// one that has to be correct.
const PgBackRestBinary = "pgbackrest"

// RenderPgBackRestConf produces the configuration for one protected postgres
// service. It contains no credential: the destination keys and the repository
// passphrase all arrive as PGBACKREST_* variables that the image's entrypoint
// reads from the mode-0600 credential file on the host.
func RenderPgBackRestConf(app, service string, target BackupTarget, policy *ProtectionPolicy) ([]byte, error) {
	database := app
	if policy == nil {
		return nil, fmt.Errorf("render pgbackrest configuration for %q: no protection policy", service)
	}
	if target.Kind != "s3-compatible" {
		return nil, fmt.Errorf("render pgbackrest configuration for %q: unsupported target kind %q", service, target.Kind)
	}
	host, port, secure, err := s3EndpointParts(target.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("render pgbackrest configuration for %q: %w", service, err)
	}
	if !secure {
		// pgBackRest speaks TLS to an S3 repository and offers no way not to:
		// its only transport options are a CA file, a CA path, and whether to
		// verify the certificate. `tls: insecure` therefore means an
		// unverified certificate, never a plaintext connection.
		//
		// Refused here rather than rendered, because the alternative is a
		// configuration that looks right and fails a minute later inside the
		// container with "TLS error [1:167772427] wrong version number" — a
		// message that says nothing about the endpoint being the problem.
		return nil, errf("recovery_objective_unsupported", "backup_targets."+policy.Target+".endpoint", "ob validate",
			"point-in-time recovery for %q needs an https endpoint: pgBackRest always uses TLS to an S3 repository, and %q is plaintext. Use https and keep tls: insecure if the certificate is self-signed",
			service, target.Endpoint)
	}

	// The prefix is anchored as an absolute path inside the bucket. pgBackRest
	// would otherwise choose its own layout, and a declared prefix that did not
	// actually contain the repository would be a prefix in name only.
	repoPath := "/" + strings.Trim(target.Prefix, "/")

	global := [][2]string{
		{"repo1-type", "s3"},
		{"repo1-path", repoPath},
		{"repo1-s3-bucket", target.Bucket},
		{"repo1-s3-endpoint", host},
	}
	if port != "" {
		global = append(global, [2]string{"repo1-s3-port", port})
	}
	// Path-style addressing, because an S3-compatible endpoint is usually not
	// the one provider whose virtual-host naming works everywhere. A bucket
	// name is then a path segment rather than a subdomain that has to resolve.
	global = append(global, [2]string{"repo1-s3-uri-style", "path"})
	if target.Region != "" {
		global = append(global, [2]string{"repo1-s3-region", target.Region})
	} else {
		// pgBackRest requires a region even where the destination ignores it.
		// The project made TLS and endpoint explicit; silently inventing a
		// region is the smaller surprise than refusing an otherwise complete
		// target, but it is written down rather than left implicit.
		global = append(global, [2]string{"repo1-s3-region", "us-east-1"})
	}
	if target.TLS == "insecure" {
		// Certificate verification off, transport still TLS. This is the only
		// thing "insecure" can mean to pgBackRest, and the plaintext reading of
		// it was refused above.
		global = append(global, [2]string{"repo1-storage-verify-tls", "n"})
	}
	global = append(global,
		// Encryption is the target's contract rather than a preference: the
		// schema refuses a policy whose recovery kind has no declared mode, so
		// a repository Onebox writes is encrypted whatever the destination does.
		[2]string{"repo1-cipher-type", "aes-256-cbc"},
		[2]string{"repo1-retention-full", fmt.Sprint(policy.Retention.MinimumGenerations)},
		// Start the backup's checkpoint immediately instead of waiting for the
		// server's own schedule, so a backup takes place at the time it was
		// scheduled for rather than up to a checkpoint interval later.
		[2]string{"start-fast", "y"},
		// Console output is the operation's evidence and is captured by the
		// caller. A log file inside the container is a second copy that nothing
		// reads, nothing ships, and nothing rotates.
		[2]string{"log-level-console", "info"},
		[2]string{"log-level-file", "off"},
	)

	var b strings.Builder
	b.WriteString("# Generated by Onebox for service " + service + ".\n")
	b.WriteString("# No credential is here: the destination keys and the repository\n")
	b.WriteString("# passphrase arrive as PGBACKREST_* variables from the mode-0600\n")
	b.WriteString("# credential file on the host.\n\n")
	b.WriteString("[global]\n")
	for _, kv := range global {
		b.WriteString(kv[0] + "=" + kv[1] + "\n")
	}
	b.WriteString("\n[" + PgBackRestStanza(app, service) + "]\n")
	b.WriteString("pg1-path=" + PgDataPath + "\n")
	// pgBackRest connects as the operating-system user it runs as unless told
	// otherwise, and that user is `postgres` — a role the driver never creates,
	// because Onebox owns the identity and makes it `onebox` so two projects on
	// one host cannot silently share a superuser. Without this line every
	// command fails at "role \"postgres\" does not exist", which reads like a
	// broken database rather than a wrong connection.
	b.WriteString("pg1-user=" + PgSuperuser + "\n")
	b.WriteString("pg1-database=" + database + "\n")
	return []byte(b.String()), nil
}

// PgBackRestCredentialEntries are the entry names the target-side credential
// file must define for this target: the two or three the project named for the
// destination, plus the fixed repository passphrase. Names only — no value is
// produced, read, or returned here.
func PgBackRestCredentialEntries(target BackupTarget) []string {
	entries := []string{
		target.Credentials.AccessKeyEntry,
		target.Credentials.SecretKeyEntry,
		PgBackRestCipherEntry,
	}
	if target.Credentials.SessionTokenEntry != "" {
		entries = append(entries, target.Credentials.SessionTokenEntry)
	}
	sort.Strings(entries)
	return entries
}

// s3EndpointParts splits a declared endpoint into the host, the optional port,
// and whether the scheme is https — pgBackRest wants the first two separately
// and cannot be given a scheme at all, so the third is reported back to the
// caller to refuse rather than silently dropped.
func s3EndpointParts(endpoint string) (host, port string, secure bool, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", false, fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Host == "" {
		return "", "", false, fmt.Errorf("endpoint %q names no host", endpoint)
	}
	return u.Hostname(), u.Port(), u.Scheme == "https", nil
}

// serviceProtection is everything renderService needs to run a service under
// protection. It is derived from observed durable lifecycle state rather than
// from the presence of a policy, for the same reason image selection is: a
// declared intent is not an established protection, and rendering a server with
// an archive_command pointing at a stanza that was never created would take the
// database down at its next WAL switch.
type serviceProtection struct {
	Stanza         string
	ConfigHostDir  string
	ConfigHostPath string
	CredentialFile string
	ArchiveCommand string
	ArchiveTimeout string
	KeyEntry       string
	SecretEntry    string
	SessionEntry   string
}

// protectionForRender returns the rendering inputs for a protected service, or
// nil when the service is not running under established protection.
//
// The gate is the observed lifecycle state, and both live states qualify:
// `enabled` is the ordinary case, and `disable-pending` still archives, because
// disablement has not completed and stopping the archive first would put a gap
// in the recovery window that the retained history claims is continuous.
func (r *Resolved) protectionForRender(n Names, serviceName string) (*serviceProtection, error) {
	state, observed := r.serviceRuntime[serviceName]
	if !observed || (state.ProtectionState != "enabled" && state.ProtectionState != "disable-pending") {
		return nil, nil
	}
	service, ok := r.Services[serviceName]
	if !ok {
		return nil, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	projection, err := r.renderProtectionProjection(serviceName, service, state)
	if err != nil {
		return nil, err
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	if driverName != "postgres" {
		// Every other driver's protection is declared in the lifecycle
		// catalogue but has no executable renderer yet. Refusing here is the
		// difference between "not implemented" and a service that runs as if
		// the policy had never been written.
		return nil, errf("backup_driver_unsupported", "services."+serviceName+".protection", "ob validate",
			"driver %q has no executable protection renderer; only postgres is executable today", driverName)
	}
	// The RPO is the reason archive_timeout exists. Without it a quiet database
	// archives only when a 16MB segment fills, so the newest recoverable point
	// can trail by hours whatever the policy says. Forcing a segment switch at
	// the declared interval is what makes maximum_data_loss a bound rather than
	// an aspiration.
	maximumDataLoss, err := PositiveDuration(projection.Policy.MaximumDataLoss)
	if err != nil {
		return nil, err
	}
	return &serviceProtection{
		Stanza:         PgBackRestStanza(r.Spec.Name, serviceName),
		ConfigHostDir:  n.ServiceProtectionConfigDir(serviceName),
		ConfigHostPath: n.ServiceProtectionConfigFile(serviceName),
		CredentialFile: n.ProtectionCredentialFile(serviceName, projection.Policy.Target),
		ArchiveCommand: PgBackRestArchiveCommand(r.Spec.Name, serviceName),
		ArchiveTimeout: fmt.Sprintf("%ds", int(maximumDataLoss.Seconds())),
		KeyEntry:       projection.Target.Credentials.AccessKeyEntry,
		SecretEntry:    projection.Target.Credentials.SecretKeyEntry,
		SessionEntry:   projection.Target.Credentials.SessionTokenEntry,
	}, nil
}

// RenderServiceProtectionConfigs generates the pgBackRest configuration for
// every service running under established protection, keyed by the host path it
// must be written to. It is separate from RenderServices because the two are
// written separately: the configuration must be in place before the container
// that mounts it starts.
func (r *Resolved) RenderServiceProtectionConfigs(env string) (map[string][]byte, error) {
	n := r.Spec.NamesFor(env)
	out := map[string][]byte{}
	for _, name := range sortedKeys(r.Spec.Services) {
		protection, err := r.protectionForRender(n, name)
		if err != nil {
			return nil, err
		}
		if protection == nil {
			continue
		}
		state := r.serviceRuntime[name]
		projection, err := r.renderProtectionProjection(name, r.Services[name], state)
		if err != nil {
			return nil, err
		}
		content, err := RenderPgBackRestConf(r.Spec.Name, name, projection.Target, &projection.Policy)
		if err != nil {
			return nil, err
		}
		out[protection.ConfigHostPath] = content
	}
	return out, nil
}

// renderProtectionProjection is the policy and target a *running* protected
// service is archiving under.
//
// The recorded projection wins over the project's current intent, and that
// ordering is the point. Enablement writes down exactly what it bound, and the
// server has been archiving to that repository ever since. If someone edits the
// policy — or deletes it — the archive does not retroactively move, so
// rendering from the edited project would point a live server at a repository
// its own history is not in. The project's intent takes effect at the next
// enablement, which is where the change can be made deliberately.
func (r *Resolved) renderProtectionProjection(serviceName string, service Service, state ServiceRuntimeState) (ProtectionEffectiveProjection, error) {
	if state.LastEffective != nil {
		return *state.LastEffective, nil
	}
	projection, _, err := r.effectiveProtectionProjection(serviceName, service)
	if err != nil {
		return ProtectionEffectiveProjection{}, errf("protection_state_incomplete", "services."+serviceName+".protection", "ob backup status "+serviceName,
			"service %s is protected but neither its durable state nor the project says what it is protected by; restore the policy or disable protection", serviceName)
	}
	return projection, nil
}
