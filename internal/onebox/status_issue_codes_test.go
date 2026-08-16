package onebox

import (
	"slices"
	"testing"
)

// statusIssueCodes derives a branchable code by matching the issue prose, so
// the two are coupled by string. A reworded issue silently falls back to
// "<component>_diverged", and for the owner record that is worse than
// unhelpful: a refused read publishes no owner, which after omitempty looks
// exactly like a genuinely unclaimed host. These are the exact sentences
// proxyReads emits — keep them in step.
func TestOwnerRefusalIssuesCarryTheirOwnCode(t *testing.T) {
	for _, issue := range []string{
		"host owner record is not a regular file; only a regular file is a valid owner record",
		"the host owner record exists but could not be read; verify the record's permissions",
		"the path that should hold the host owner record is not a directory",
		"the host state directory cannot be searched, so the owner record could not be read",
	} {
		codes := statusIssueCodes("proxy", []string{issue})
		if !slices.Contains(codes, "host_owner_unreadable") {
			t.Errorf("issue %q derived %v; a refused owner read must not be indistinguishable from an unclaimed host", issue, codes)
		}
	}
}

// The same argument as the owner record: a refused applied-config read gates
// ConfigDiverged to false and omits the hash, so without a code of its own a
// consumer sees no drift and no marker — indistinguishable from a host in
// sync. These are the exact sentences statusFileIssue emits.
func TestRefusedFileReadsCarryTheirOwnCodes(t *testing.T) {
	for issue, want := range map[string]string{
		"the applied configuration exists but could not be read; verify the file and its permissions":                                                                                        "applied_config_unreadable",
		"the applied configuration could not be read: the path that should hold /var/lib/ob/_host/proxy/config.hash is not a directory":                                                      "applied_config_unreadable",
		"the applied configuration could not be read: the directory holding /var/lib/ob/_host/proxy/config.hash cannot be searched, so a missing file cannot be told from an unreadable one": "applied_config_unreadable",
		"the certificate store exists but could not be read; verify the file and its permissions":                                                                                            "certificate_store_unreadable",
		"the certificate store could not be read: the path that should hold /var/lib/ob/_host/proxy/acme/acme.json is not a directory":                                                       "certificate_store_unreadable",
		"certificate store is unreadable": "certificate_store_unreadable",
	} {
		if codes := statusIssueCodes("proxy", []string{issue}); !slices.Contains(codes, want) {
			t.Errorf("issue %q derived %v, want %s", issue, codes, want)
		}
	}
}

// An empty record was read successfully; it is not unreadable. Sending a
// consumer down a permissions remedy for a file that reads perfectly well is a
// different wrong answer from the one the unreadable code exists to give.
func TestAnEmptyOwnerRecordIsNotReportedAsUnreadable(t *testing.T) {
	codes := statusIssueCodes("proxy", []string{"host owner record is present but empty; an empty record is not a valid claim"})
	if slices.Contains(codes, "host_owner_unreadable") {
		t.Errorf("an empty record derived %v, which points at permissions", codes)
	}
	if !slices.Contains(codes, "host_owner_empty") {
		t.Errorf("an empty record derived %v, want host_owner_empty", codes)
	}
}

// A record that reads fine but names nothing valid is a third condition: not
// unreadable (no permission change helps) and not empty (there is content).
func TestAnInvalidOwnerNameIsNotReportedAsUnreadable(t *testing.T) {
	codes := statusIssueCodes("proxy", []string{"the host owner record is not a valid application name; every mutation will refuse this host"})
	if slices.Contains(codes, "host_owner_unreadable") {
		t.Errorf("an invalid owner name derived %v, which points at permissions", codes)
	}
	if !slices.Contains(codes, "host_owner_invalid") {
		t.Errorf("an invalid owner name derived %v, want host_owner_invalid", codes)
	}
}

// The generic fallback must still apply to anything else, or the code above
// would swallow unrelated proxy issues.
func TestUnrelatedProxyIssuesKeepTheGenericCode(t *testing.T) {
	codes := statusIssueCodes("proxy", []string{"something else entirely"})
	if !slices.Contains(codes, "proxy_diverged") {
		t.Errorf("unrelated issue derived %v, want proxy_diverged", codes)
	}
}
