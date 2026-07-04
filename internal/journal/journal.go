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
	DeployID   string `json:"deploy_id"`
	Epoch      int    `json:"epoch"`
	Phase      string `json:"phase"`
	SubStep    string `json:"sub_step,omitempty"`
	Role       string `json:"role,omitempty"`
	Event      string `json:"event"`            // start | intent | result | finish | abort
	Status     string `json:"status,omitempty"` // ok | fail
	Detail     string `json:"detail,omitempty"`
	TS         string `json:"ts"`
	Operator   string `json:"operator,omitempty"`
	GitSHA     string `json:"git_sha,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
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
	DeployID    string
	Epoch       int
	Started     bool
	Finished    bool
	Failed      bool
	Aborted     bool
	PrevRelease string
	GateOpen    bool
	Done        map[string]bool // "transfer", "migrate", "release:<role>"
	Operator    string
	GitSHA      string
	StartedAt   string
}

func Summarize(recs []Record) Summary {
	s := Summary{Done: map[string]bool{}}
	for _, r := range recs {
		s.DeployID = r.DeployID
		if r.Epoch > s.Epoch {
			s.Epoch = r.Epoch
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
				s.GateOpen = strings.Contains(r.Detail, "changed=false")
			case strings.HasPrefix(r.SubStep, "job:"):
				s.Done[r.SubStep] = true
			case r.Phase == "release" && r.Role != "":
				s.Done["release:"+r.Role] = true
			}
		}
	}
	return s
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
