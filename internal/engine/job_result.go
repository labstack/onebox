package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/labstack/onebox/internal/journal"
)

const maxJobResultBytes = 64 << 10

var (
	errJobResultMissing = errors.New("job result is missing")
	jobResultIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)
)

type jobResultDocument struct {
	SchemaVersion   string   `json:"schema_version"`
	Changed         *bool    `json:"changed"`
	Provider        string   `json:"provider,omitempty"`
	BeforeRevisions []string `json:"before_revisions,omitempty"`
	AfterRevisions  []string `json:"after_revisions,omitempty"`
}

func parseJobResult(raw []byte) (journal.JobResultEvidence, error) {
	if len(raw) > maxJobResultBytes {
		return journal.JobResultEvidence{}, fmt.Errorf("job result exceeds %d bytes", maxJobResultBytes)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return journal.JobResultEvidence{}, errJobResultMissing
	}
	var (
		document jobResultDocument
		err      error
	)
	if raw[0] == '{' {
		document, err = parseJSONJobResult(raw)
	} else {
		document, err = parseKeyValueJobResult(string(raw))
	}
	if err != nil {
		return journal.JobResultEvidence{}, err
	}
	if err := validateJobResult(document); err != nil {
		return journal.JobResultEvidence{}, err
	}
	evidence := journal.JobResultEvidence{
		SchemaVersion:   journal.JobResultSchemaVersion,
		Changed:         *document.Changed,
		Provider:        document.Provider,
		BeforeRevisions: append([]string(nil), document.BeforeRevisions...),
		AfterRevisions:  append([]string(nil), document.AfterRevisions...),
	}
	canonical, err := json.Marshal(evidence)
	if err != nil {
		return journal.JobResultEvidence{}, err
	}
	evidence.Digest = HashBytes(canonical)
	return evidence, nil
}

func parseJSONJobResult(raw []byte) (jobResultDocument, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document jobResultDocument
	if err := decoder.Decode(&document); err != nil {
		return jobResultDocument{}, fmt.Errorf("decode job result: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return jobResultDocument{}, errors.New("decode job result: multiple JSON values")
		}
		return jobResultDocument{}, fmt.Errorf("decode job result: %w", err)
	}
	return document, nil
}

func parseKeyValueJobResult(raw string) (jobResultDocument, error) {
	document := jobResultDocument{SchemaVersion: journal.JobResultSchemaVersion}
	var changedSeen, beforeSeen, afterSeen bool
	for lineNumber, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return jobResultDocument{}, fmt.Errorf("job result line %d is not key=value", lineNumber+1)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "schema_version":
			document.SchemaVersion = value
		case "changed":
			if changedSeen {
				return jobResultDocument{}, errors.New("job result changed is declared more than once")
			}
			changedSeen = true
			changed := false
			switch value {
			case "true":
				changed = true
			case "false":
			default:
				return jobResultDocument{}, errors.New("job result changed must be true or false")
			}
			document.Changed = &changed
		case "provider":
			document.Provider = value
		case "before_revision":
			beforeSeen = true
			document.BeforeRevisions = append(document.BeforeRevisions, value)
		case "after_revision":
			afterSeen = true
			document.AfterRevisions = append(document.AfterRevisions, value)
		case "before_revisions":
			beforeSeen = true
			document.BeforeRevisions = append(document.BeforeRevisions, splitRevisionList(value)...)
		case "after_revisions":
			afterSeen = true
			document.AfterRevisions = append(document.AfterRevisions, splitRevisionList(value)...)
		default:
			return jobResultDocument{}, fmt.Errorf("job result contains unsupported key %q", key)
		}
	}
	if document.Provider != "" {
		if !beforeSeen || !afterSeen {
			return jobResultDocument{}, errors.New("provider job result requires before_revisions and after_revisions")
		}
		if document.BeforeRevisions == nil {
			document.BeforeRevisions = []string{}
		}
		if document.AfterRevisions == nil {
			document.AfterRevisions = []string{}
		}
	}
	return document, nil
}

func splitRevisionList(value string) []string {
	if value == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func validateJobResult(document jobResultDocument) error {
	if document.SchemaVersion != journal.JobResultSchemaVersion {
		return fmt.Errorf("unsupported job result schema %q; want %s", document.SchemaVersion, journal.JobResultSchemaVersion)
	}
	if document.Changed == nil {
		return errors.New("job result changed is required")
	}
	if document.Provider == "" {
		if document.BeforeRevisions != nil || document.AfterRevisions != nil {
			return errors.New("job result revisions require a provider")
		}
		return nil
	}
	if !jobResultIdentifier.MatchString(document.Provider) {
		return errors.New("job result provider is invalid")
	}
	if document.BeforeRevisions == nil || document.AfterRevisions == nil {
		return errors.New("provider job result requires before_revisions and after_revisions")
	}
	if len(document.BeforeRevisions) > 256 || len(document.AfterRevisions) > 256 {
		return errors.New("job result contains too many revisions")
	}
	for _, revision := range append(append([]string(nil), document.BeforeRevisions...), document.AfterRevisions...) {
		if !jobResultIdentifier.MatchString(revision) {
			return errors.New("job result contains an invalid revision identifier")
		}
	}
	if !uniqueRevisionList(document.BeforeRevisions) || !uniqueRevisionList(document.AfterRevisions) {
		return errors.New("job result provider revision lists must not contain duplicates")
	}
	changed := !equalRevisionLists(document.BeforeRevisions, document.AfterRevisions)
	if changed != *document.Changed {
		return errors.New("job result changed disagrees with provider revision evidence")
	}
	if document.Provider == "atlas" && !revisionListPrefix(document.BeforeRevisions, document.AfterRevisions) {
		return errors.New(`"atlas" after_revisions must extend before_revisions without rewriting history`)
	}
	return nil
}

func uniqueRevisionList(revisions []string) bool {
	seen := make(map[string]struct{}, len(revisions))
	for _, revision := range revisions {
		if _, exists := seen[revision]; exists {
			return false
		}
		seen[revision] = struct{}{}
	}
	return true
}

func revisionListPrefix(prefix, values []string) bool {
	if len(prefix) > len(values) {
		return false
	}
	for i := range prefix {
		if prefix[i] != values[i] {
			return false
		}
	}
	return true
}

func equalRevisionLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
