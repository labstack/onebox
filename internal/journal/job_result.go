package journal

const JobResultSchemaVersion = "onebox.run/job-result/v1alpha1"

// JobResultEvidence is the redaction-safe, normalized result of a pre-release
// job. Provider revisions are identifiers only; commands, connection strings,
// and result-file contents never enter the journal.
type JobResultEvidence struct {
	SchemaVersion   string   `json:"schema_version"`
	Changed         bool     `json:"changed"`
	Provider        string   `json:"provider,omitempty"`
	BeforeRevisions []string `json:"before_revisions,omitempty"`
	AfterRevisions  []string `json:"after_revisions,omitempty"`
	Digest          string   `json:"digest"`
}
