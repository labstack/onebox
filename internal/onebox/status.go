package onebox

// Staged work, kept deliberately. Nothing outside this file calls into it —
// statusIssueCodes and statusDigest are reached only from tests, and
// canonicalStatus only from statusDigest. That is a state a dead-code pass
// reads as "delete me", and it would be wrong twice over:
//
//   - `statusIssueCodes` is one half of a contract. The other half is the issue
//     prose in internal/engine/proxystatus.go, whose comment says every sentence
//     leads with its component *because* this function matches on it. Delete
//     this and the reason that prose is stable disappears with it.
//   - The rest is status output written against a structured status shape the
//     engine does not yet emit, for the same proposal that
//     DeploymentProposal.Preconditions belongs to.
//
// Whether that work is still coming is a product question, tracked with the
// rest of the part-built surface. Until it is answered, this file stays, and
// this comment is here so the next person to run `unused` does not have to
// rediscover why.
//
// See #63.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/engine"
)

func statusIssueCodes(component string, issues []string) []string {
	out := make([]string, 0, len(issues))
	seen := map[string]bool{}
	for _, issue := range issues {
		code := component + "_diverged"
		switch {
		case strings.HasPrefix(issue, "replica count is "):
			code = "replica_count_mismatch"
		case strings.Contains(issue, "no release is recorded"):
			code = "release_not_recorded"
		case strings.Contains(issue, "not onebox-deployed"):
			code = "unmanaged_container"
		case strings.Contains(issue, "runs release "):
			code = "release_mismatch"
		case strings.Contains(issue, "health is "), strings.Contains(issue, " is unhealthy"), strings.Contains(issue, " is starting"), strings.Contains(issue, " is down"):
			code = "container_health_unready"
		case issue == "not running":
			code = "not_running"
		case issue == "local and applied configuration hashes differ":
			code = "configuration_drift"
		case strings.HasPrefix(issue, "certificate renewal is overdue"):
			code = "certificate_renewal_overdue"
		case issue == "certificate store is unreadable",
			strings.HasPrefix(issue, "the certificate store"):
			code = "certificate_store_unreadable"
		// A refused applied-config read also gates ConfigDiverged to false and
		// omits the hash, so without its own code a JSON consumer sees no
		// drift, no unreadable marker, and the same generic code an unhealthy
		// container gets.
		case strings.HasPrefix(issue, "the applied configuration"):
			code = "applied_config_unreadable"
		// A refused owner read publishes no owner, which is byte-identical to
		// a genuinely unclaimed host once omitempty drops the empty field. A
		// caller keying on the absent field would read "unclaimed" and propose
		// a bootstrap the engine then refuses, so the refusal needs a code of
		// its own rather than only prose plus complete:false.
		// Read successfully, and empty. A permissions remedy would send the
		// operator after a file that reads perfectly well.
		case strings.HasPrefix(issue, "host owner record is present but empty"):
			code = "host_owner_empty"
		// Read fine, but not a name any mutation will accept. Distinct from
		// unreadable: no permission change helps, the record's content is
		// what is wrong.
		case strings.HasPrefix(issue, "the host owner record is not a valid application name"):
			code = "host_owner_invalid"
		case strings.HasPrefix(issue, "host owner record is not a regular file"),
			strings.HasPrefix(issue, "the host owner record"),
			strings.HasPrefix(issue, "the path that should hold the host owner record"),
			strings.HasPrefix(issue, "the host state directory cannot be searched"):
			code = "host_owner_unreadable"
		}
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}

func canonicalStatus(status engine.StatusSnapshot) engine.StatusSnapshot {
	status.CapturedAt = time.Time{}
	status.Warnings = append([]engine.StatusWarning(nil), status.Warnings...)
	for i := range status.Warnings {
		status.Warnings[i].Message = ""
	}
	if status.Proxy != nil {
		proxyCopy := *status.Proxy
		proxyCopy.Certificates = append([]engine.StatusCertificate(nil), status.Proxy.Certificates...)
		for i := range proxyCopy.Certificates {
			// DaysRemaining is a presentation countdown. NotAfter and the overdue
			// threshold state carry the operational fact without daily digest noise.
			proxyCopy.Certificates[i].DaysRemaining = 0
		}
		status.Proxy = &proxyCopy
	}
	return status
}

func statusDigest(status engine.StatusSnapshot) (string, error) {
	encoded, err := json.Marshal(canonicalStatus(status))
	if err != nil {
		return "", err
	}
	return engine.HashBytes(encoded), nil
}
