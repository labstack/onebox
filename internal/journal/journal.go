// Package journal is the deploy journal (design §05): append-only JSONL at
// /var/lib/ob/<app>/journal/<deploy-id>.jsonl, one sync per record. It is
// the mechanism behind resume, abort, fencing forensics, and audit — a spec,
// not a noun.
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

type Record struct {
	DeployID     string `json:"deploy_id"`
	Epoch        int    `json:"epoch"`
	Phase        string `json:"phase"`
	SubStep      string `json:"sub_step,omitempty"`
	Role         string `json:"role,omitempty"`
	Event        string `json:"event"`            // start | intent | result | finish | abort
	Status       string `json:"status,omitempty"` // ok | fail
	Detail       string `json:"detail,omitempty"`
	RollbackSafe bool   `json:"rollback_safe,omitempty"`
	// RollbackPolicySafe records that the interrupted deploy's own policy
	// covers this effect even when it cannot prove a no-op result. Keeping this
	// on the journal prevents a later config edit from weakening abort safety.
	RollbackPolicySafe bool   `json:"rollback_policy_safe,omitempty"`
	TS                 string `json:"ts"`
	Operator           string `json:"operator,omitempty"`
	GitSHA             string `json:"git_sha,omitempty"`
	ConfigHash         string `json:"config_hash,omitempty"`
}

type Writer struct {
	T          transport.Transport
	App        string
	DeployID   string
	Epoch      int
	Operator   string
	GitSHA     string
	ConfigHash string
}

func dir(app string) string      { return release.PathsFor(app).Base + "/journal" }
func file(app, id string) string { return dir(app) + "/" + id + ".jsonl" }

func DefaultOperator() string {
	user := os.Getenv("USER")
	host, _ := os.Hostname()
	return user + "@" + host
}

// Append writes one record with a sync — the journal survives a host crash
// up to the last completed step.
func (w *Writer) Append(ctx context.Context, r Record) error {
	r.DeployID, r.Epoch = w.DeployID, w.Epoch
	r.TS = time.Now().UTC().Format(time.RFC3339)
	if r.Operator == "" {
		r.Operator = w.Operator
	}
	if r.GitSHA == "" {
		r.GitSHA = w.GitSHA
	}
	if r.ConfigHash == "" {
		r.ConfigHash = w.ConfigHash
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f := file(w.App, w.DeployID)
	cmd := "mkdir -p " + q(dir(w.App)) + " && printf '%s\\n' " + q(string(b)) + " >> " + q(f) + " && sync " + q(f)
	res, err := w.T.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("journal append: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Read returns the records of one deploy; unparseable lines are tolerated
// (the journal is forensic — a torn write must not block recovery).
func Read(ctx context.Context, t transport.Transport, app, deployID string) ([]Record, error) {
	res, err := t.Run(ctx, "cat "+q(file(app, deployID))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) == nil && r.DeployID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// List returns deploy ids with journals, oldest first (ids sort by time).
func List(ctx context.Context, t transport.Transport, app string) ([]string, error) {
	res, err := t.Run(ctx, "ls -1 "+q(dir(app))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasSuffix(l, ".jsonl") {
			ids = append(ids, strings.TrimSuffix(l, ".jsonl"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// journalMarker prefixes each file's contents in the bulk read below. Journal
// records are single-line JSON objects (they start with '{'), so a line
// starting with this marker is unambiguous.
const journalMarker = "@@ob-journal@@"

// Journals returns every deploy's records keyed by id, plus the ids oldest
// first, in a SINGLE round trip. FindIncomplete previously ran one `cat` per
// journal (O(deploys) round trips against a high-latency host, paid in full
// whenever no deploy is incomplete); a per-file marker lets one command carry
// them all while parsing/Summarize stays here, unchanged. (Audit still reads
// per-file — it is not on the status hot path.)
func Journals(ctx context.Context, t transport.Transport, app string) ([]string, map[string][]Record, error) {
	// A missing journal directory is a valid never-deployed state. Existing but
	// unreadable directories/files fail so status cannot report false completeness.
	// The `echo` after each `cat` guarantees a newline before the next marker:
	// a crash can leave a journal's last record un-terminated, and without it
	// that record's line would swallow the following file's marker (design §05:
	// a torn write must not corrupt recovery).
	cmd := "if [ -d " + q(dir(app)) + " ]; then cd " + q(dir(app)) + " || exit; " +
		"for f in *.jsonl; do [ -f \"$f\" ] || continue; echo " + q(journalMarker) +
		"\"$f\"; cat \"$f\" || exit; echo; done; elif [ -e " + q(dir(app)) + " ]; then exit 2; fi"
	res, err := t.Run(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	if res.ExitCode != 0 {
		return nil, nil, fmt.Errorf("read deployment journals failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	byID := map[string][]Record{}
	var ids []string
	cur := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(line), journalMarker); ok {
			cur = strings.TrimSuffix(f, ".jsonl")
			if _, seen := byID[cur]; !seen {
				byID[cur] = nil
				ids = append(ids, cur)
			}
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || cur == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) == nil && r.DeployID != "" {
			byID[cur] = append(byID[cur], r)
		}
	}
	sort.Strings(ids) // ids are lexically time-ordered by construction
	return ids, byID, nil
}

// PruneCandidates returns journal ids beyond the keep window, oldest first.
// A journal outlives its release (design §05): keep is typically 2× the
// release retention.
func PruneCandidates(ctx context.Context, t transport.Transport, app string, keep int) ([]string, error) {
	ids, err := List(ctx, t, app)
	if err != nil || len(ids) <= keep {
		return nil, err
	}
	return ids[:len(ids)-keep], nil
}

// Summary is the journal reduced to what resume/abort/audit need.
type Summary struct {
	DeployID string
	Epoch    int
	Started  bool
	Finished bool
	Failed   bool
	Aborted  bool
	// DeploySucceeded is sticky and operation-scoped: later maintenance records
	// may share this journal id, but cannot erase a compatible code activation.
	DeploySucceeded bool
	PrevRelease     string
	GateOpen        bool
	// RollbackCovered is true only when every recorded effect attempt was
	// either explicitly rollback-safe or covered by the interrupted deploy's
	// own policy declaration.
	RollbackCovered bool
	Done            map[string]bool // "transfer", "migrate", "job:<service>", "hook:<name>", "release:<role>", DoneGateRecorded
	Operator        string
	GitSHA          string
	StartedAt       string
}

// DoneGateRecorded marks that this journal contains a rollback-effect
// decision. Resume uses it to preserve the aggregate result across retries.
const DoneGateRecorded = "gate:recorded"

// EffectBaselineSubStep is written durably before transfer or any effect can
// start. Later job/hook attempts join the aggregate and may close it; if the
// runner dies during upload, the journal still proves rollback is safe.
const EffectBaselineSubStep = "gate:baseline"

type effectAttempt struct {
	resolved   bool
	explicitOK bool
	policyOK   bool
}

type effectAttemptKey struct {
	subStep string
	epoch   int
}

func Summarize(recs []Record) Summary {
	s := Summary{Done: map[string]bool{}}
	var attempts []effectAttempt
	active := map[effectAttemptKey]int{}
	for _, r := range recs {
		s.DeployID = r.DeployID
		if r.Epoch > s.Epoch {
			s.Epoch = r.Epoch
		}
		effectStep := r.SubStep == "migrate" || r.SubStep == EffectBaselineSubStep ||
			strings.HasPrefix(r.SubStep, "job:") || strings.HasPrefix(r.SubStep, "hook:")
		if effectStep {
			s.Done[DoneGateRecorded] = true
			key := effectAttemptKey{subStep: r.SubStep, epoch: r.Epoch}
			switch r.Event {
			case "intent":
				// Every intent is a distinct attempt. If a runner disappears and a
				// later epoch retries the same job, the unresolved first attempt stays
				// in the aggregate and can never be erased by the retry's result.
				attempts = append(attempts, effectAttempt{policyOK: r.RollbackPolicySafe})
				active[key] = len(attempts) - 1
			case "result":
				idx, ok := active[key]
				if !ok {
					// Legacy journals sometimes contain only the completed result.
					attempts = append(attempts, effectAttempt{})
					idx = len(attempts) - 1
				}
				attempts[idx].resolved = true
				attempts[idx].explicitOK = r.Status == "ok" && recordRollbackSafe(r)
				attempts[idx].policyOK = attempts[idx].policyOK || r.RollbackPolicySafe
				delete(active, key)
			}
		}

		switch r.Event {
		case "start":
			s.Started = true
			s.Operator, s.GitSHA, s.StartedAt = r.Operator, r.GitSHA, r.TS
			if v, ok := strings.CutPrefix(r.Detail, "prev="); ok {
				s.PrevRelease = v
			}
		case "finish":
			s.Finished = true
			if r.Phase == "deploy" && r.Status == "ok" {
				s.DeploySucceeded = true
			}
			if r.Status == "fail" {
				s.Failed = true
			}
		case "abort":
			s.Aborted = true
		case "result":
			if r.Status != "ok" {
				continue
			}
			switch {
			case r.Phase == "transfer":
				s.Done["transfer"] = true
			case r.SubStep == "migrate": // legacy journals (pre auto-run jobs)
				s.Done["migrate"] = true
			case strings.HasPrefix(r.SubStep, "job:"):
				s.Done[r.SubStep] = true
			case strings.HasPrefix(r.SubStep, "hook:"):
				s.Done[r.SubStep] = true
			case r.Phase == "release" && r.Role != "":
				s.Done["release:"+r.Role] = true
			}
		}
	}
	if len(attempts) > 0 {
		s.GateOpen = true
		s.RollbackCovered = true
		for _, attempt := range attempts {
			explicitSafe := attempt.resolved && attempt.explicitOK
			if !explicitSafe {
				s.GateOpen = false
			}
			if !explicitSafe && !attempt.policyOK {
				s.RollbackCovered = false
			}
		}
	}
	return s
}

func recordRollbackSafe(r Record) bool {
	if r.RollbackSafe {
		return true
	}
	// Recognize journals written before rollback_safe became explicit.
	detail := strings.TrimSpace(r.Detail)
	return detail == "changed=false" || strings.Contains(detail, "rollback-safe by data_effect=none")
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
