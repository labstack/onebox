// Package journal implements the append-only deploy journal at
// /var/lib/ob/<app>/journal/<deploy-id>.jsonl, one sync per record. It is
// the mechanism behind resume, abort, fencing forensics, and audit — a spec,
// not a noun.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/shellquote"
	"github.com/labstack/onebox/internal/transport"
)

// MigrationBackupEvidence is the secret-free authorization accepted before a
// migration. Receipt mode identifies validated backup artifacts; override mode
// retains the explicit break-glass operator, reason, and time.
type MigrationBackupEvidence struct {
	Mode               string   `json:"mode"` // receipt | override
	ReceiptDigest      string   `json:"receipt_digest,omitempty"`
	OverrideDigest     string   `json:"override_digest,omitempty"`
	ProtectedResources []string `json:"protected_resources"`
	ValidUntil         string   `json:"valid_until"`
	RecordedBy         string   `json:"recorded_by,omitempty"`
	RecordedAt         string   `json:"recorded_at,omitempty"`
	OverrideOperator   string   `json:"override_operator,omitempty"`
	OverrideReason     string   `json:"override_reason,omitempty"`
	OverrideCreatedAt  string   `json:"override_created_at,omitempty"`
	OverrideSource     string   `json:"override_source,omitempty"`
}

type Record struct {
	DeployID     string `json:"deploy_id"`
	Epoch        int    `json:"epoch"`
	Phase        string `json:"phase"`
	SubStep      string `json:"sub_step,omitempty"`
	Role         string `json:"role,omitempty"`
	Event        string `json:"event"`            // start | intent | result | finish | abort
	Status       string `json:"status,omitempty"` // ok | fail
	Detail       string `json:"detail,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	RollbackSafe bool   `json:"rollback_safe,omitempty"`
	// RollbackPolicySafe records that the interrupted deploy's own policy
	// covers this effect even when it cannot prove a no-op result. Keeping this
	// on the journal prevents a later config edit from weakening abort safety.
	RollbackPolicySafe      bool                     `json:"rollback_policy_safe,omitempty"`
	TS                      string                   `json:"ts"`
	Operator                string                   `json:"operator,omitempty"`
	GitSHA                  string                   `json:"git_sha,omitempty"`
	ConfigHash              string                   `json:"config_hash,omitempty"`
	ApprovalDigest          string                   `json:"approval_digest,omitempty"`
	ApprovalClass           string                   `json:"approval_class,omitempty"`
	ApprovedBy              string                   `json:"approved_by,omitempty"`
	ApprovalSource          string                   `json:"approval_source,omitempty"`
	AllowUnknownMigration   bool                     `json:"allow_unknown_migration,omitempty"`
	Runner                  *buildinfo.Runner        `json:"runner,omitempty"`
	MigrationBackupRequired bool                     `json:"migration_backup_required,omitempty"`
	MigrationBackup         *MigrationBackupEvidence `json:"migration_backup,omitempty"`
	JobResult               *JobResultEvidence       `json:"job_result,omitempty"`
	// Protection fields share the host-synced operation journal.
	OperationKind       string                    `json:"operation_kind,omitempty"`
	Service             string                    `json:"service,omitempty"`
	ProtectionStepID    string                    `json:"protection_step_id,omitempty"`
	ProtectionAttempt   int                       `json:"protection_attempt,omitempty"`
	IncompleteResources []IncompleteResource      `json:"incomplete_resources,omitempty"`
	Retry               *RetryClassification      `json:"retry,omitempty"`
	HelperProvenance    *HelperProvenance         `json:"helper_provenance,omitempty"`
	TerminalResult      *ProtectionTerminalResult `json:"terminal_result,omitempty"`
	// Exec invocation evidence is intentionally value-free: command bytes and
	// passthrough output never cross the durable journal boundary.
	Target        string `json:"target,omitempty"`
	TargetKind    string `json:"target_kind,omitempty"`
	CommandDigest string `json:"command_digest,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type Writer struct {
	T transport.Transport
	// Names carries the resolved layout, so a journal is written where the
	// release it describes actually lives.
	Names                   app.Names
	DeployID                string
	Epoch                   int
	Operator                string
	GitSHA                  string
	ConfigHash              string
	ApprovalDigest          string
	ApprovalClass           string
	ApprovedBy              string
	ApprovalSource          string
	AllowUnknownMigration   bool
	Runner                  *buildinfo.Runner
	MigrationBackupRequired bool
	MigrationBackup         *MigrationBackupEvidence
}

func dir(n app.Names) string             { return release.PathsFor(n).Base + "/journal" }
func file(n app.Names, id string) string { return dir(n) + "/" + id + ".jsonl" }

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
	// Command stderr can contain decrypted credentials or provider responses.
	// It remains on the trusted local error path and never becomes durable
	// evidence. Journal failures retain a stable taxonomy and phase/sub-step.
	if r.Status == "fail" {
		r.Detail = "operation failed; inspect trusted local diagnostics"
		if r.ErrorCode == "" {
			r.ErrorCode = "execution_failed"
		}
	}
	if r.Operator == "" {
		r.Operator = w.Operator
	}
	if r.GitSHA == "" {
		r.GitSHA = w.GitSHA
	}
	if r.ConfigHash == "" {
		r.ConfigHash = w.ConfigHash
	}
	// Durable authorization context belongs on deploy and manual-job starts (so
	// a crash before the first protected step remains resumable) and on the
	// explicit migration-backup authorization decision. Avoid copying
	// operator-provided fields onto every journal line.
	authorizationRecord := (r.Phase == "deploy" || r.Phase == "job") && r.Event == "start" || r.SubStep == MigrationBackupSubStep
	if authorizationRecord {
		if r.ApprovalDigest == "" {
			r.ApprovalDigest = w.ApprovalDigest
		}
		if r.ApprovalClass == "" {
			r.ApprovalClass = w.ApprovalClass
		}
		if r.ApprovedBy == "" {
			r.ApprovedBy = w.ApprovedBy
		}
		if r.ApprovalSource == "" {
			r.ApprovalSource = w.ApprovalSource
		}
		if w.AllowUnknownMigration {
			r.AllowUnknownMigration = true
		}
	}
	if r.Runner == nil {
		r.Runner = w.Runner
	}
	if w.MigrationBackupRequired {
		r.MigrationBackupRequired = true
	}
	if authorizationRecord && r.MigrationBackup == nil {
		r.MigrationBackup = w.MigrationBackup
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f := file(w.Names, w.DeployID)
	cmd := "mkdir -p " + q(dir(w.Names)) + " && printf '%s\\n' " + q(string(b)) + " >> " + q(f) + " && sync " + q(f)
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
func Read(ctx context.Context, t transport.Transport, n app.Names, deployID string) ([]Record, error) {
	res, err := t.Run(ctx, "cat "+q(file(n, deployID))+" 2>/dev/null || true")
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
func List(ctx context.Context, t transport.Transport, n app.Names) ([]string, error) {
	res, err := t.Run(ctx, "ls -1 "+q(dir(n))+" 2>/dev/null || true")
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
// first, in a SINGLE round trip. FindIncomplete is the caller that needs this
// shape: one `cat` per journal would cost O(deploys) round trips against a
// high-latency host, paid in full even when no deploy is incomplete. A per-file
// marker lets one command carry them all while parsing and Summarize stay here.
// (Audit reads per-file — it is not on the status hot path.)
func Journals(ctx context.Context, t transport.Transport, n app.Names) ([]string, map[string][]Record, error) {
	// A missing journal directory is a valid never-deployed state. Existing but
	// unreadable directories/files fail so status cannot report false completeness.
	// -e follows symlinks, so the -L arm is what keeps a dangling journal-dir
	// link out of the "never deployed" answer — the false completeness above.
	// The `echo` after each `cat` guarantees a newline before the next marker:
	// a crash can leave a journal's last record un-terminated, and without it
	// that record's line would swallow the following file's marker, losing an
	// entire deploy's records to one torn write.
	cmd := "if [ -d " + q(dir(n)) + " ]; then cd " + q(dir(n)) + " || exit; " +
		// Searchable but not readable: cd succeeds and the glob cannot
		// enumerate, so the loop never runs and the read looks like a
		// never-deployed host. Same false completeness, one step over.
		"[ -r . ] || exit 2; " +
		"for f in *.jsonl; do if [ ! -f \"$f\" ]; then " +
		// Any non-regular entry is the directory's problem one level down:
		// skipping it drops that deploy's records, and FindIncomplete then
		// reports nothing incomplete — the same false completeness. -e
		// catches a directory or device sitting where a journal belongs;
		// -L catches the dangling link -e cannot see.
		"if [ -e \"$f\" ] || [ -L \"$f\" ]; then exit 2; fi; continue; fi; echo " + q(journalMarker) +
		"\"$f\"; cat \"$f\" || exit; echo; done; elif [ -e " + q(dir(n)) + " ] || [ -L " + q(dir(n)) + " ]; then exit 2; else " +
		// An unsearchable ancestor hides the directory as thoroughly as a
		// missing one, and answering "never deployed" there strands an
		// interrupted deploy: FindIncomplete reports nothing to resume.
		app.UndeterminedArm(dir(n)) + "true; fi"
	res, err := t.Run(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	// The script's own refusals carry no stderr, so the generic form below
	// would print an exit code and no cause.
	switch res.ExitCode {
	case 2:
		return nil, nil, fmt.Errorf("read deployment journals failed: %s exists but a journal there could not be read; inspect the deployment state directory", dir(n))
	case app.ProbeStatePathNotDirectory:
		return nil, nil, fmt.Errorf("read deployment journals failed: the path that should hold %s is not a directory; inspect the deployment state directory", dir(n))
	case app.ProbeUndetermined:
		return nil, nil, fmt.Errorf("read deployment journals failed: a directory holding %s cannot be searched, so a never-deployed host cannot be told from an unreadable one; verify access, then retry", dir(n))
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

// PruneCandidates returns journal ids beyond independent deploy and auxiliary
// keep windows, oldest first. High-frequency exec/job/service activity must not
// evict the deploy history needed for recovery and audit.
func PruneCandidates(ctx context.Context, t transport.Transport, n app.Names, keep int) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("journal retention keep window must be positive")
	}
	ids, byID, err := Journals(ctx, t, n)
	if err != nil {
		return nil, err
	}
	var deployIDs, auxiliaryIDs []string
	for _, id := range ids {
		records := byID[id]
		if len(records) == 0 {
			return nil, fmt.Errorf("journal retention refused: %s has no usable records", id)
		}
		deploy := false
		for _, record := range records {
			if record.Phase == "deploy" && record.Event == "start" {
				deploy = true
				break
			}
		}
		if deploy {
			deployIDs = append(deployIDs, id)
		} else {
			auxiliaryIDs = append(auxiliaryIDs, id)
		}
	}
	var victims []string
	if len(deployIDs) > keep {
		victims = append(victims, deployIDs[:len(deployIDs)-keep]...)
	}
	if len(auxiliaryIDs) > keep {
		victims = append(victims, auxiliaryIDs[:len(auxiliaryIDs)-keep]...)
	}
	sort.Strings(victims)
	return victims, nil
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
	// Recovered is a successful automatic rollback terminal. The deploy itself
	// failed, but no resume/abort work remains after centralized recovery.
	Recovered   bool
	PrevRelease string
	GateOpen    bool
	// RollbackCovered is true only when every recorded effect attempt was
	// either explicitly rollback-safe or covered by the interrupted deploy's
	// own policy declaration.
	RollbackCovered         bool
	Done                    map[string]bool // "transfer", "migrate", "job:<service>", "hook:<name>", "release:<role>", DoneGateRecorded
	Operator                string
	GitSHA                  string
	StartedAt               string
	MigrationBackupRequired bool
	MigrationBackup         *MigrationBackupEvidence
	ApprovalDigest          string
	ApprovalClass           string
	ApprovedBy              string
	ApprovalSource          string
	AllowUnknownMigration   bool
	JobResults              map[string]JobResultEvidence
}

// DoneGateRecorded marks that this journal contains a rollback-effect
// decision. Resume uses it to preserve the aggregate result across retries.
const DoneGateRecorded = "gate:recorded"

// DoneActivation marks a durably recorded successful activation. It is the
// evidence that separates the two halves of a deploy: before it, an interrupted
// operation never took effect and resume replays the choreography; after it,
// the release is the truth and only the post-activation steps remain.
const DoneActivation = "activation"

// FinalizeSubStepPrefix marks the post-activation steps. They are journaled
// individually — and deliberately not as rollback effects, which belong to the
// choreography that runs before activation — so a finalize replay repeats none
// that already recorded a successful result.
const FinalizeSubStepPrefix = "finalize:"

// EffectBaselineSubStep is written durably before transfer or any effect can
// start. Later job/hook attempts join the aggregate and may close it; if the
// runner dies during upload, the journal still proves rollback is safe.
const EffectBaselineSubStep = "gate:baseline"

// MigrationBackupSubStep is the durable marker that a receipt or explicit
// override was accepted before a pending migration job could start.
const MigrationBackupSubStep = "migration-backup:evidence"

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
	s := Summary{Done: map[string]bool{}, JobResults: map[string]JobResultEvidence{}}
	var attempts []effectAttempt
	active := map[effectAttemptKey]int{}
	for _, r := range recs {
		s.DeployID = r.DeployID
		if r.Epoch > s.Epoch {
			s.Epoch = r.Epoch
		}
		if r.MigrationBackup != nil {
			s.MigrationBackup = r.MigrationBackup
		}
		if r.MigrationBackupRequired {
			s.MigrationBackupRequired = true
		}
		if r.ApprovalDigest != "" {
			s.ApprovalDigest = r.ApprovalDigest
			s.ApprovalClass = r.ApprovalClass
			s.ApprovedBy = r.ApprovedBy
			s.ApprovalSource = r.ApprovalSource
		}
		if r.AllowUnknownMigration {
			s.AllowUnknownMigration = true
		}
		if r.JobResult != nil && strings.HasPrefix(r.SubStep, "job:") {
			s.JobResults[strings.TrimPrefix(r.SubStep, "job:")] = *r.JobResult
		}
		effectStep := r.SubStep == EffectBaselineSubStep ||
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
					// Sparse journals sometimes contain only the completed result.
					attempts = append(attempts, effectAttempt{})
					idx = len(attempts) - 1
				}
				attempts[idx].resolved = true
				attempts[idx].explicitOK = r.Status == "ok" && recordRollbackSafe(r)
				attempts[idx].policyOK = attempts[idx].policyOK || r.RollbackPolicySafe
				delete(active, key)
			}
		}

		deployRecord := r.Phase == "deploy"
		switch r.Event {
		case "start":
			if deployRecord {
				// Only the FIRST start describes what this operation superseded.
				// Every resume appends its own start with the live current
				// pointer, and once activation has moved that pointer it names
				// the release being resumed — so a later start would rewrite the
				// operation's predecessor to itself, and abort and finalize both
				// read this field as durable evidence of what came before.
				if !s.Started {
					s.Operator, s.GitSHA, s.StartedAt = r.Operator, r.GitSHA, r.TS
					if v, ok := strings.CutPrefix(r.Detail, "prev="); ok {
						s.PrevRelease = v
					}
				}
				s.Started = true
			}
		case "finish":
			if deployRecord && r.Status == "ok" {
				s.Finished = true
				s.DeploySucceeded = true
			} else if r.Status == "fail" {
				s.Failed = true
			}
		case "abort":
			if r.Status == "ok" {
				s.Aborted = true
			} else if r.Status == "fail" {
				s.Failed = true
			}
		case "result":
			if r.Status != "ok" {
				continue
			}
			if r.Phase == "auto-rollback" {
				s.Recovered = true
			}
			switch {
			case r.Phase == "transfer":
				s.Done["transfer"] = true
			case r.Phase == "activation":
				s.Done[DoneActivation] = true
			case strings.HasPrefix(r.SubStep, FinalizeSubStepPrefix):
				s.Done[r.SubStep] = true
			case r.SubStep == MigrationBackupSubStep:
				s.Done[MigrationBackupSubStep] = true
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

func q(s string) string { return shellquote.Quote(s) }
