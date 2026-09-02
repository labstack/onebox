package onebox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

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
		// A relative bind source is release content: the rendered service
		// names the path, never the bytes behind it, so without this a
		// workload whose config files changed would look unchanged. The
		// digest is what lets the planner retain such a workload when the
		// content really is identical, instead of recreating every one of
		// them on every deploy.
		for order, volume := range workload.Volumes {
			if !volume.IsBind() || path.IsAbs(volume.Source) {
				continue
			}
			summary, err := bindMountSummary(staging, volume.Source)
			if err != nil {
				return nil, fmt.Errorf("fingerprint bind mount %q for workload %q: %w", volume.Source, name, err)
			}
			inputs = append(inputs, struct {
				Kind    string `json:"kind"`
				Order   int    `json:"order"`
				Path    string `json:"path"`
				Content string `json:"content"`
			}{"bind_mount", order, volume.Source, digestBytes(summary)})
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

// bindMountSummary reduces a staged bind source to one sorted line per file:
// path, permission bits and a content digest. A retained container keeps the
// directory it was created with wholesale, so anything left out of this summary
// is invisible for the life of that container — which is why the mode is here
// and not only the content, and why a non-regular entry contributes its type
// rather than being skipped.
func bindMountSummary(staging, source string) ([]byte, error) {
	root := filepath.Join(staging, filepath.FromSlash(strings.TrimPrefix(source, "./")))
	var lines []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			lines = append(lines, fmt.Sprintf("%s|%04o|%s", filepath.ToSlash(rel), info.Mode().Perm(), info.Mode().Type()))
			return nil
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s|%04o|%s", filepath.ToSlash(rel), info.Mode().Perm(), digestBytes(body)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n")), nil
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
