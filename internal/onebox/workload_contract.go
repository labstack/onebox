package onebox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/labstack/onebox/internal/app"
)

// workloadContracts builds value-free identities for startup inputs that are
// not fully represented in the rendered Compose service. File contents are
// reduced to a digest before they enter the aggregate; secret inputs use only
// an opaque identity supplied by the deployment transaction.
func workloadContracts(cfg *app.Resolved, staging, projectRoot string, secretRevisions map[string]string) (map[string]app.WorkloadContract, error) {
	secretInputs, err := cfg.SecretInputRevisions(projectRoot)
	if err != nil {
		return nil, err
	}
	contracts := make(map[string]app.WorkloadContract, len(cfg.Workloads))
	for _, name := range sortedNames(cfg.Workloads) {
		workload := cfg.Workloads[name]
		inputs := make([]any, 0)
		for order, entry := range cfg.Spec.EnvFilesFor(workload) {
			if entry.Encrypted() {
				continue
			}
			body, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(entry.StagedPath())))
			if err != nil {
				return nil, fmt.Errorf("fingerprint startup file for workload %q: %w", name, err)
			}
			inputs = append(inputs, struct {
				Kind    string `json:"kind"`
				Order   int    `json:"order"`
				Path    string `json:"path"`
				Content string `json:"content"`
			}{"env_file", order, entry.StagedPath(), digestBytes(body)})
		}
		for order, need := range workload.Needs {
			service, ok := cfg.Services[need.Name]
			if !ok {
				continue
			}
			client, ok := cfg.Spec.ClientEnvFor(need.Name)
			if !ok {
				return nil, fmt.Errorf("fingerprint managed connection %q for workload %q: driver is unknown", need.Name, name)
			}
			inputs = append(inputs, struct {
				Kind    string        `json:"kind"`
				Order   int           `json:"order"`
				Need    any           `json:"need"`
				Service any           `json:"service"`
				Client  app.ClientEnv `json:"client"`
			}{"managed_connection", order, need, service, client})
		}
		contract := app.WorkloadContract{SecretRevision: secretRevisions[name], SecretInputRevision: secretInputs[name]}
		if len(inputs) > 0 {
			encoded, err := json.Marshal(inputs)
			if err != nil {
				return nil, fmt.Errorf("encode startup inputs for workload %q: %w", name, err)
			}
			contract.StartupRevision = digestBytes(encoded)
		}
		contracts[name] = contract
	}
	return contracts, nil
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func secretRevisionsFor(graph []app.SecretDeclaration, fallback string, existing map[string]app.WorkloadContract) map[string]string {
	revisions := map[string]string{}
	for _, declaration := range graph {
		for _, workload := range declaration.AffectedWorkloads {
			if revision := existing[workload].SecretRevision; app.IsSecretGeneration(revision) {
				revisions[workload] = revision
			} else {
				revisions[workload] = fallback
			}
		}
	}
	return revisions
}

func markChangedSecretWorkloads(revisions map[string]string, changed map[string]bool, generation string) {
	for workload := range changed {
		revisions[workload] = generation
	}
}
