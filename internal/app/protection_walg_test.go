package app

import (
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
				Retention: BackupRetention{MinimumGenerations: tc.minimum, RecoveryWindow: tc.window},
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
	recorded := ProtectionEffectiveProjection{
		Policy: BackupPolicy{Target: "original", RecoveryKind: "pitr", MaximumDataLoss: "15m"},
		Target: BackupTarget{Kind: "s3-compatible", Bucket: "recorded-bucket", Endpoint: "https://a.example.net"},
	}
	edited := &Resolved{
		Spec: &Spec{
			Name: "shop", BasePath: "/var/lib/ob",
			Services: map[string]Service{"db": {Driver: "postgres", Version: 18, Backup: &BackupPolicy{
				Target: "moved", RecoveryKind: "pitr", MaximumDataLoss: "15m",
			}}},
			BackupTargets: map[string]BackupTarget{"moved": {Kind: "s3-compatible", Bucket: "new-bucket", Endpoint: "https://b.example.net"}},
		},
		Env: "production",
	}
	bound, err := edited.WithServiceRuntimeStates(map[string]ServiceRuntimeState{
		"db": {ProtectionState: "enabled", ServiceImage: "postgres@sha256:" + strings.Repeat("a", 64),
			PublicationVerified: true, DigestAvailable: true, LastEffective: &recorded},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bound.EffectiveProtectionProjection("db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Bucket != "recorded-bucket" || got.Policy.Target != "original" {
		t.Fatalf("projection = %#v, want the recorded one — an edited target must not redirect a restore", got)
	}
}

// The repository prefix must be injective. Hyphens are legal in both an app and
// a service name, so a hyphen join would land app `a-b`/service `c` and app
// `a`/service `b-c` on one prefix and interleave two clusters' backups. The
// prefix is unversioned, so this cannot be corrected later.
func TestWalgPrefixCannotCollideAcrossHyphenatedNames(t *testing.T) {
	target := BackupTarget{Bucket: "backups", Prefix: "production"}
	first := WalgPrefix(target, "a-b", "c")
	second := WalgPrefix(target, "a", "b-c")
	if first == second {
		t.Fatalf("two distinct services share the repository prefix %q", first)
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
