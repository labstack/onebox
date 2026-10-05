package engine

import (
	"io"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestArchivingIssuesDoNotTreatUnusableTimeoutsAsWithinPolicy(t *testing.T) {
	for _, tc := range []struct {
		timeout string
		issue   string
		policy  string
	}{
		{timeout: "900s"},
		{timeout: "15min"},
		{timeout: "14min"},
		{timeout: "16min", issue: "closes a write-ahead log segment"},
		{timeout: "106752d", issue: "cannot determine"},
		{timeout: "213504d", issue: "cannot determine"},
		{timeout: "9223372037s", issue: "cannot determine"},
		{timeout: "9223372036855ms", issue: "cannot determine"},
		{timeout: "153722868min", issue: "cannot determine"},
		{timeout: "2562048h", issue: "cannot determine"},
		{timeout: "soon", issue: "cannot determine"},
		{timeout: "0", issue: "disabled"},
		{timeout: "900s", issue: "cannot determine", policy: "213504d"},
		{timeout: "900s", issue: "cannot determine", policy: "0"},
		{timeout: "900s", issue: "cannot determine", policy: "soon"},
	} {
		t.Run(tc.timeout+"/"+tc.policy, func(t *testing.T) {
			policy := tc.policy
			if policy == "" {
				policy = "15m"
			}
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.Contains(command, "show archive_timeout;") {
					return transport.Result{Stdout: "on\n" + app.WalgBinary + " wal-push %p\n" + tc.timeout + "\n"}, true
				}
				return transport.Result{}, false
			}}
			spec := &app.Spec{
				Name: "shop",
				Services: map[string]app.Service{
					"database": {Driver: "postgres", Version: "18", Backup: &app.BackupPolicy{Target: "offsite", MaxDataLoss: policy}},
				},
				BackupTargets: map[string]app.BackupTarget{"offsite": {}},
			}
			e := New(&app.Resolved{Spec: spec, Env: "production"}, nil, fake, Options{Out: io.Discard})
			issues, err := e.archivingIssues(t.Context(), "database")
			if err != nil {
				t.Fatal(err)
			}
			if tc.issue == "" {
				if len(issues) != 0 {
					t.Fatalf("valid timeout raised issues: %v", issues)
				}
			} else if len(issues) != 1 || !strings.Contains(issues[0], tc.issue) {
				t.Fatalf("timeout %q silently accepted or misreported: %v", tc.timeout, issues)
			}
		})
	}
}
