package app

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// SecretDeclaration is the value-free contract that binds secret mutation to
// the release runtime. It records where encrypted input comes from, where the
// runtime reads it, its declaration order/scope, and every affected workload.
type SecretDeclaration struct {
	ID                string                  `json:"id"`
	SourceFile        string                  `json:"source_file"`
	Provider          string                  `json:"provider"`
	OutputPath        string                  `json:"output_path"`
	Scope             string                  `json:"scope"`
	Order             int                     `json:"order"`
	AffectedWorkloads []string                `json:"affected_workloads"`
	ProjectionEntries []SecretProjectionEntry `json:"projection_entries,omitempty"`
}

type SecretProjectionEntry struct {
	Destination string `json:"destination"`
	Source      string `json:"source"`
}

// SecretDeclarationGraph returns a deterministic, value-free graph. Equality
// is exact: adding, removing, reordering, moving scope, changing provider, or
// changing the affected workload set all require a deploy before secret push.
func (p *Spec) SecretDeclarationGraph() []SecretDeclaration {
	if p == nil {
		return nil
	}
	byIdentity := map[string]*SecretDeclaration{}
	for _, workloadName := range sortedKeys(p.Workloads) {
		workload := p.Workloads[workloadName]
		entries, scope := p.secretEnvFilesFor(workloadName, workload)
		for order, entry := range entries {
			if !entry.Encrypted() {
				continue
			}
			identity := scope + "\x00" + entry.File + "\x00" + entry.Provider + "\x00" + entry.StagedPath() + "\x00" + strconv.Itoa(order)
			declaration := byIdentity[identity]
			if declaration == nil {
				declaration = &SecretDeclaration{
					ID:         secretDeclarationID(identity),
					SourceFile: entry.File, Provider: entry.Provider, OutputPath: entry.StagedPath(),
					Scope: scope, Order: order,
				}
				byIdentity[identity] = declaration
			}
			declaration.AffectedWorkloads = append(declaration.AffectedWorkloads, workloadName)
		}

		for order, projection := range p.ExternalConnectionProjections(workloadName, workload) {
			projectionEntries := make([]SecretProjectionEntry, 0, len(projection.Entries))
			for destination, source := range projection.Entries {
				projectionEntries = append(projectionEntries, SecretProjectionEntry{Destination: destination, Source: source})
			}
			sort.Slice(projectionEntries, func(i, j int) bool {
				return projectionEntries[i].Destination < projectionEntries[j].Destination
			})
			identity := "external:" + workloadName + "\x00" + projection.Path
			byIdentity[identity] = &SecretDeclaration{
				ID:         secretDeclarationID(identity),
				SourceFile: projection.Source.File, Provider: projection.Source.Provider, OutputPath: projection.Path,
				Scope: "external:" + workloadName, Order: order,
				AffectedWorkloads: []string{workloadName}, ProjectionEntries: projectionEntries,
			}
		}
	}
	out := make([]SecretDeclaration, 0, len(byIdentity))
	for _, declaration := range byIdentity {
		sort.Strings(declaration.AffectedWorkloads)
		out = append(out, *declaration)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		if out[i].OutputPath != out[j].OutputPath {
			return out[i].OutputPath < out[j].OutputPath
		}
		return out[i].SourceFile < out[j].SourceFile
	})
	return out
}

func secretDeclarationID(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return "secret_" + hex.EncodeToString(digest[:6])
}

func (p *Spec) secretEnvFilesFor(workloadName string, workload Workload) ([]EnvFile, string) {
	if workload.EnvFiles != nil {
		return workload.EnvFiles, "workload:" + workloadName
	}
	if !roleTakesTheDefault(workload.Role) {
		return nil, ""
	}
	if p.envDefault != nil {
		return p.envDefault, "environment-default"
	}
	if p.Runtime != nil {
		return p.Runtime.EnvFiles, "runtime-default"
	}
	return nil, ""
}

func (r *Resolved) SecretDeclarationGraph() []SecretDeclaration {
	if r == nil {
		return nil
	}
	return r.Spec.SecretDeclarationGraph()
}
