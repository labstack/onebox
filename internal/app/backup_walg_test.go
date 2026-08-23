package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Retention has two floors and the larger one wins. A frequent schedule under a
// long window needs far more generations than the declared minimum, and keeping
// only the minimum silently shortens the window the policy promised.
func TestWalgRetainCountSatisfiesBothRetentionFloors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cron        string
		window      string
		minimum     int
		wantAtLeast int
	}{
		{"frequent schedule is bound by the window", "*/5 * * * *", "24h", 2, 288},
		{"daily schedule over a week", "0 2 * * *", "168h", 2, 7},
		{"sparse schedule falls back to the declared minimum", "0 2 * * *", "1h", 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WalgRetainCount(BackupPolicy{
				Schedule:  Schedule{Cron: tc.cron},
				Retention: BackupRetention{Keep: tc.minimum, Window: tc.window},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got < tc.wantAtLeast {
				t.Fatalf("retain count = %d, want at least %d to honour both floors", got, tc.wantAtLeast)
			}
			if got < tc.minimum {
				t.Fatalf("retain count %d is below the declared minimum %d", got, tc.minimum)
			}
		})
	}
}

// The recorded projection wins over the project's current intent.
//
// Enablement writes down which repository it bound, and the server has archived
// there ever since. An operator editing backup_targets afterwards must not
// silently redirect a restore at a repository the history is not in — nor at a
// credential file installed under the old target's name.
func TestRecordedProjectionWinsOverEditedIntent(t *testing.T) {
	recorded := BackupEffectiveProjection{
		Policy: BackupPolicy{Target: "original", RecoveryKind: "pitr", MaxDataLoss: "15m"},
		Target: BackupTarget{Kind: "s3-compatible", Bucket: "recorded-bucket", Endpoint: "https://a.example.net"},
	}
	edited := &Resolved{
		Spec: &Spec{
			Name: "shop", BasePath: "/var/lib/ob",
			Services: map[string]Service{"db": {Driver: "postgres", Version: 18, Backup: &BackupPolicy{
				Target: "moved", RecoveryKind: "pitr", MaxDataLoss: "15m",
			}}},
			BackupTargets: map[string]BackupTarget{"moved": {Kind: "s3-compatible", Bucket: "new-bucket", Endpoint: "https://b.example.net"}},
		},
		Env: "production",
	}
	bound, err := edited.WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"db": {BackupState: "enabled", ServiceImage: "postgres@sha256:" + strings.Repeat("a", 64),
			PublicationVerified: true, DigestAvailable: true, LastEffective: &recorded,
			DatabaseSystemIdentifier: "7513211627332151223", BackupRepositoryGeneration: "7513211627332151223"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bound.EffectiveBackupProjection("db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Bucket != "recorded-bucket" || got.Policy.Target != "original" {
		t.Fatalf("projection = %#v, want the recorded one — an edited target must not redirect a restore", got)
	}
	backup, err := bound.backupForRender(bound.Spec.NamesFor("production"), "db")
	if err != nil {
		t.Fatal(err)
	}
	wantRepository := "s3://recorded-bucket/shop_db/clusters/7513211627332151223"
	if repository := backup.Environment["WALG_S3_PREFIX"]; repository != wantRepository {
		t.Fatalf("rendered repository = %q, want recorded generation %q", repository, wantRepository)
	}
}

// The repository prefix must be injective. Hyphens are legal in both an app and
// a service name, so a hyphen join would land app `a-b`/service `c` and app
// `a`/service `b-c` on one prefix and interleave two clusters' backups. The
// prefix is unversioned, so this cannot be corrected later.
func TestWalgPrefixCannotCollideAcrossHyphenatedNames(t *testing.T) {
	target := BackupTarget{Bucket: "backups", Prefix: "production"}
	first := WalgPrefix(target, "a-b", "c", "1")
	second := WalgPrefix(target, "a", "b-c", "1")
	if first == second {
		t.Fatalf("two distinct services share the repository prefix %q", first)
	}
}

func TestWalgPrefixSeparatesSuccessiveDatabaseClusters(t *testing.T) {
	target := BackupTarget{Bucket: "backups", Prefix: "production"}
	first := WalgPrefix(target, "shop", "database", "7513211627332151223")
	second := WalgPrefix(target, "shop", "database", "7513211627332151224")
	if first == second {
		t.Fatalf("successive PostgreSQL clusters share repository %q", first)
	}
	if !strings.HasSuffix(first, "/clusters/7513211627332151223") {
		t.Fatalf("cluster-scoped prefix = %q", first)
	}
	legacy := WalgPrefix(target, "shop", "database", "")
	if strings.Contains(legacy, "/clusters/") {
		t.Fatalf("legacy repository was silently relocated: %q", legacy)
	}
}

// A quoted value is ordinary in a shell-sourced dotenv and is stripped when the
// file is installed, so validation must judge the same form. Judging the quoted
// text rejected a perfectly good key for being 66 characters, with a message
// about hex that said nothing true.
func TestQuotedCredentialValuesAreAccepted(t *testing.T) {
	target := BackupTarget{Credentials: CredentialReference{
		AccessKeyEntry: "K", SecretKeyEntry: "S", File: "secrets/backup.env",
	}}
	plaintext := []byte("export K=\"key\"\nS='secret'\n" + WalgRepositoryKeyEntry + "=\"" + strings.Repeat("ab", 32) + "\"\n")
	if err := ValidateWalgCredentials(plaintext, target); err != nil {
		t.Fatalf("quoted credential file rejected: %v", err)
	}
}

// The wrapper is the only place that can put a trust store in wal-g's
// environment: it runs inside the driver's image, which ships none.
func TestTheWrapperPointsWalgAtTheStagedTrustStore(t *testing.T) {
	wrapper := string(RenderWalgWrapper(BackupTarget{
		Credentials: CredentialReference{
			AccessKeyEntry: "BACKUP_ACCESS_KEY_ID",
			SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
		},
	}))

	// Three layers can do the verifying, and which one does depends on the
	// endpoint: wal-g's own S3 setting, the AWS SDK beneath it, and crypto/tls
	// beneath that. Naming only one leaves the other two on an empty store.
	for _, name := range []string{"WALG_S3_CA_CERT_FILE", "AWS_CA_BUNDLE", "SSL_CERT_FILE"} {
		if !strings.Contains(wrapper, "export "+name) {
			t.Errorf("wrapper never exports %s, so an HTTPS endpoint cannot be verified:\n%s", name, wrapper)
		}
		if !strings.Contains(wrapper, name+"="+WalgTrustStore) {
			t.Errorf("%s does not point at the staged bundle %s", name, WalgTrustStore)
		}
	}
	// Guarded, not unconditional: a host with no bundle should leave the
	// image's own store in play rather than name a path that is not there.
	if !strings.Contains(wrapper, "if [ -r "+WalgTrustStore+" ]; then") {
		t.Errorf("the trust store is exported without checking it was staged:\n%s", wrapper)
	}
}

// runWrapper executes a rendered wrapper with a stubbed wal-g on PATH, so the
// test observes what the wrapper actually does rather than what it says.
func runWrapper(t *testing.T, target BackupTarget, env []string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	// A stub standing in for the staged binary, reporting whether the wrapper
	// handed it an encryption key. The wrapper execs wal-g by its absolute
	// staged path, so only that one line is redirected; everything the guard
	// does above it runs verbatim.
	stub := filepath.Join(dir, "wal-g")
	body := "#!/bin/sh\nprintf 'libsodium=[%s]\\n' \"${WALG_LIBSODIUM_KEY-}\"\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	rendered := strings.Replace(string(RenderWalgWrapper(target)), WalgMountPath+"/wal-g", stub, 1)
	script := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(script, []byte(rendered), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func walgTestTarget() BackupTarget {
	return BackupTarget{Credentials: CredentialReference{
		AccessKeyEntry: "BACKUP_ACCESS_KEY_ID",
		SecretKeyEntry: "BACKUP_SECRET_ACCESS_KEY",
	}}
}

// A credential file written by an older runner names the entries that runner
// required. If a required entry is simply absent, the wrapper must refuse to
// run: wal-g without WALG_LIBSODIUM_KEY does not fail, it writes the backup
// unencrypted — and a backup that is silently unencrypted is worse than one
// that visibly did not happen.
func TestTheWrapperRefusesToRunWithoutARequiredCredential(t *testing.T) {
	_, stderr, code := runWrapper(t, walgTestTarget(), []string{
		"BACKUP_ACCESS_KEY_ID=key",
		"BACKUP_SECRET_ACCESS_KEY=secret",
		// the repository key is missing, as it is after a namespace change
	})
	if code == 0 {
		t.Fatal("the wrapper ran wal-g with no repository key, so the backup would be unencrypted")
	}
	if !strings.Contains(stderr, WalgRepositoryKeyEntry) {
		t.Fatalf("the failure does not name the missing entry: %q", stderr)
	}
}

func TestTheWrapperRefusesToRunWithoutDestinationCredentials(t *testing.T) {
	for _, missing := range []string{"BACKUP_ACCESS_KEY_ID", "BACKUP_SECRET_ACCESS_KEY"} {
		t.Run(missing, func(t *testing.T) {
			env := []string{
				"BACKUP_ACCESS_KEY_ID=key",
				"BACKUP_SECRET_ACCESS_KEY=secret",
				WalgRepositoryKeyEntry + "=" + strings.Repeat("ab", 32),
			}
			var kept []string
			for _, entry := range env {
				if !strings.HasPrefix(entry, missing+"=") {
					kept = append(kept, entry)
				}
			}
			if _, stderr, code := runWrapper(t, walgTestTarget(), kept); code == 0 {
				t.Fatalf("the wrapper ran with %s unset", missing)
			} else if !strings.Contains(stderr, missing) {
				t.Fatalf("the failure does not name %s: %q", missing, stderr)
			}
		})
	}
}

func TestTheWrapperPassesTheRepositoryKeyToWalg(t *testing.T) {
	key := strings.Repeat("ab", 32)
	stdout, stderr, code := runWrapper(t, walgTestTarget(), []string{
		"BACKUP_ACCESS_KEY_ID=key",
		"BACKUP_SECRET_ACCESS_KEY=secret",
		WalgRepositoryKeyEntry + "=" + key,
	})
	if code != 0 {
		t.Fatalf("wrapper exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "libsodium=["+key+"]") {
		t.Fatalf("wal-g did not receive the repository key: %q", stdout)
	}
}
