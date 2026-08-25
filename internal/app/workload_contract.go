package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// WorkloadStartupRevisionLabel binds startup-only, non-secret inputs that are
	// not represented by the rendered Compose service itself. The value is a
	// one-way aggregate; source values never enter the runtime or plan.
	WorkloadStartupRevisionLabel = "ob.startup-revision"
	// WorkloadSecretRevisionLabel is an opaque per-workload secret identity. It
	// deliberately differs from ob.secret-generation: a generation is the
	// transaction-wide storage slot, while this identity changes only for the
	// workloads whose effective secret inputs changed.
	WorkloadSecretRevisionLabel = "ob.secret-revision"
	// WorkloadSecretInputRevisionLabel identifies the encrypted source material
	// and value-free projection declaration that produced a workload's secrets.
	WorkloadSecretInputRevisionLabel = "ob.secret-input-revision"
)

var workloadStartupRevision = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// WorkloadContract is the material outside the rendered Compose service that
// decides whether recreating a container can change its startup configuration.
// It contains identities only, never environment or secret values.
type WorkloadContract struct {
	StartupRevision     string
	SecretRevision      string
	SecretInputRevision string
}

// ApplyWorkloadContracts stamps the materialized runtime with its per-workload
// startup identities and recomputes the stable workload revision. Secret
// generation paths are normalized while hashing: Compose reads those files only
// while creating a container, so moving identical bytes into a new release or
// transaction generation is not itself a runtime change.
func ApplyWorkloadContracts(composeBytes []byte, graph []SecretDeclaration, contracts map[string]WorkloadContract) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(composeBytes, &document); err != nil {
		return nil, fmt.Errorf("parse runtime workload contracts: %w", err)
	}
	services, ok := document["services"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("runtime services are malformed")
	}
	outputsByWorkload := secretOutputsByWorkload(graph)
	for workload, contract := range contracts {
		service, ok := services[workload].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("runtime service %q is malformed", workload)
		}
		if contract.StartupRevision != "" && !workloadStartupRevision.MatchString(contract.StartupRevision) {
			return nil, fmt.Errorf("runtime service %q startup revision is invalid", workload)
		}
		if contract.SecretRevision != "" && !IsSecretGeneration(contract.SecretRevision) {
			return nil, fmt.Errorf("runtime service %q secret revision is invalid", workload)
		}
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			service["labels"] = labels
		}
		setOptionalContractLabel(labels, WorkloadStartupRevisionLabel, contract.StartupRevision)
		setOptionalContractLabel(labels, WorkloadSecretRevisionLabel, contract.SecretRevision)
		if contract.SecretInputRevision != "" && !workloadStartupRevision.MatchString(contract.SecretInputRevision) {
			return nil, fmt.Errorf("runtime service %q secret input revision is invalid", workload)
		}
		setOptionalContractLabel(labels, WorkloadSecretInputRevisionLabel, contract.SecretInputRevision)
		if err := stampWorkloadRevisionWithSecretOutputs(service, outputsByWorkload[workload]); err != nil {
			return nil, fmt.Errorf("runtime service %q workload revision: %w", workload, err)
		}
	}
	return marshalDeterministic(document)
}

// WorkloadContractsFromCompose reads only the value-free contract labels. It is
// used to preserve an unchanged workload's secret identity across a later
// application release or a transaction that changes another workload.
func WorkloadContractsFromCompose(composeBytes []byte) (map[string]WorkloadContract, error) {
	var document struct {
		Services map[string]struct {
			Labels map[string]any `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeBytes, &document); err != nil {
		return nil, fmt.Errorf("parse runtime workload contracts: %w", err)
	}
	contracts := make(map[string]WorkloadContract, len(document.Services))
	for workload, service := range document.Services {
		contract := WorkloadContract{
			StartupRevision:     labelString(service.Labels, WorkloadStartupRevisionLabel),
			SecretRevision:      labelString(service.Labels, WorkloadSecretRevisionLabel),
			SecretInputRevision: labelString(service.Labels, WorkloadSecretInputRevisionLabel),
		}
		contracts[workload] = contract
	}
	return contracts, nil
}

func labelString(labels map[string]any, name string) string {
	value, ok := labels[name]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// SecretInputRevisions fingerprints encrypted source bytes plus their
// value-free projection declaration. Ciphertext hashes are safe to persist;
// decrypted values and their hashes never leave the trusted staging path.
func (r *Resolved) SecretInputRevisions(root string) (map[string]string, error) {
	graph := r.SecretDeclarationGraph()
	sources := map[string][]byte{}
	for _, declaration := range graph {
		if _, ok := sources[declaration.SourceFile]; ok {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(declaration.SourceFile)))
		if err != nil {
			return nil, fmt.Errorf("fingerprint encrypted source %q: %w", declaration.SourceFile, err)
		}
		sources[declaration.SourceFile] = body
	}
	return r.SecretInputRevisionsFromSources(sources)
}

// SecretInputRevisionsFromSources fingerprints already-snapshotted encrypted
// sources. It lets secret push bind the revision to the exact bytes it
// decrypted instead of rereading mutable project files later.
func (r *Resolved) SecretInputRevisionsFromSources(sources map[string][]byte) (map[string]string, error) {
	records := map[string][]any{}
	for _, declaration := range r.SecretDeclarationGraph() {
		body, ok := sources[declaration.SourceFile]
		if !ok {
			return nil, fmt.Errorf("fingerprint encrypted source %q: snapshot is missing", declaration.SourceFile)
		}
		digest := workloadContractDigest(body)
		record := struct {
			ID         string                  `json:"id"`
			SourceFile string                  `json:"source_file"`
			Provider   string                  `json:"provider"`
			OutputPath string                  `json:"output_path"`
			Scope      string                  `json:"scope"`
			Order      int                     `json:"order"`
			Projection []SecretProjectionEntry `json:"projection,omitempty"`
			Content    string                  `json:"content"`
		}{declaration.ID, declaration.SourceFile, declaration.Provider, declaration.OutputPath, declaration.Scope, declaration.Order, declaration.ProjectionEntries, digest}
		for _, workload := range declaration.AffectedWorkloads {
			records[workload] = append(records[workload], record)
		}
	}
	revisions := make(map[string]string, len(records))
	for workload, workloadRecords := range records {
		encoded, err := json.Marshal(workloadRecords)
		if err != nil {
			return nil, fmt.Errorf("encode secret inputs for workload %q: %w", workload, err)
		}
		revisions[workload] = workloadContractDigest(encoded)
	}
	return revisions, nil
}

func workloadContractDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func setOptionalContractLabel(labels map[string]any, name, value string) {
	if value == "" {
		delete(labels, name)
		return
	}
	labels[name] = value
}

func secretOutputsByWorkload(graph []SecretDeclaration) map[string]map[string]bool {
	outputs := map[string]map[string]bool{}
	for _, declaration := range graph {
		for _, workload := range declaration.AffectedWorkloads {
			if outputs[workload] == nil {
				outputs[workload] = map[string]bool{}
			}
			outputs[workload][declaration.OutputPath] = true
		}
	}
	return outputs
}

func normalizeSecretGenerationEnvFiles(raw any, outputs map[string]bool) any {
	normalize := func(value string) string {
		for output := range outputs {
			if secretEnvPathMatches(value, output) {
				return output
			}
		}
		return value
	}
	switch entries := raw.(type) {
	case []any:
		out := make([]any, len(entries))
		for index, entry := range entries {
			switch value := entry.(type) {
			case string:
				out[index] = normalize(value)
			case map[string]any:
				clone := make(map[string]any, len(value))
				for key, item := range value {
					clone[key] = item
				}
				if file, ok := clone["path"].(string); ok {
					clone["path"] = normalize(file)
				}
				out[index] = clone
			default:
				out[index] = entry
			}
		}
		return out
	case []string:
		out := make([]string, len(entries))
		for index, entry := range entries {
			out[index] = normalize(entry)
		}
		return out
	default:
		return raw
	}
}
