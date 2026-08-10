package app

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

const SecretGenerationDirectory = ".ob-secret-generations"

// SecretGenerationPath is the release-relative path selected by a generated
// secret runtime. The generation is opaque; the path never incorporates
// content-derived material.
func SecretGenerationPath(generation, outputPath string) string {
	return path.Join(SecretGenerationDirectory, generation, outputPath)
}

// ApplySecretGeneration produces the generation-specific Compose runtime used
// for force replacement and, once verified, committed as the release runtime.
// Only declared secret paths and their affected workloads change.
func ApplySecretGeneration(composeBytes []byte, graph []SecretDeclaration, generation string) ([]byte, error) {
	if strings.TrimSpace(generation) == "" || strings.ContainsAny(generation, "/\\") {
		return nil, fmt.Errorf("secret generation %q is invalid", generation)
	}
	var document map[string]any
	if err := yaml.Unmarshal(composeBytes, &document); err != nil {
		return nil, fmt.Errorf("parse runtime for secret generation: %w", err)
	}
	services, ok := document["services"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("runtime services are malformed")
	}
	outputsByWorkload := map[string]map[string]bool{}
	for _, declaration := range graph {
		for _, workload := range declaration.AffectedWorkloads {
			if outputsByWorkload[workload] == nil {
				outputsByWorkload[workload] = map[string]bool{}
			}
			outputsByWorkload[workload][declaration.OutputPath] = true
		}
	}
	for workload, outputs := range outputsByWorkload {
		service, ok := services[workload].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("runtime service %q is malformed", workload)
		}
		if raw, present := service["env_file"]; present {
			rewritten, err := rewriteGenerationEnvFiles(raw, outputs, generation)
			if err != nil {
				return nil, fmt.Errorf("runtime service %q env_file: %w", workload, err)
			}
			service["env_file"] = rewritten
		}
		if err := setGenerationLabel(service, generation); err != nil {
			return nil, fmt.Errorf("runtime service %q labels: %w", workload, err)
		}
	}
	return marshalDeterministic(document)
}

// SecretGenerationFromCompose reads the generation selected by every affected
// workload. An entirely unlabelled runtime has no deployed generation; partial
// or disagreeing labels are state divergence and fail closed.
func SecretGenerationFromCompose(composeBytes []byte, workloads []string) (string, error) {
	var document map[string]any
	if err := yaml.Unmarshal(composeBytes, &document); err != nil {
		return "", fmt.Errorf("parse runtime secret generation: %w", err)
	}
	services, ok := document["services"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("runtime services are malformed")
	}
	selected := ""
	unlabelled := 0
	for _, workload := range workloads {
		service, ok := services[workload].(map[string]any)
		if !ok {
			return "", fmt.Errorf("runtime service %q is malformed", workload)
		}
		generation, err := generationLabel(service["labels"])
		if err != nil {
			return "", fmt.Errorf("runtime service %q labels: %w", workload, err)
		}
		if generation == "" {
			unlabelled++
			continue
		}
		if selected != "" && selected != generation {
			return "", fmt.Errorf("runtime workloads select different secret generations")
		}
		selected = generation
	}
	if selected != "" && unlabelled > 0 {
		return "", fmt.Errorf("runtime workloads only partially select a secret generation")
	}
	return selected, nil
}

func rewriteGenerationEnvFiles(raw any, outputs map[string]bool, generation string) (any, error) {
	rewrite := func(value string) string {
		for output := range outputs {
			if secretEnvPathMatches(value, output) {
				return SecretGenerationPath(generation, output)
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
				out[index] = rewrite(value)
			case map[string]any:
				clone := make(map[string]any, len(value))
				for key, item := range value {
					clone[key] = item
				}
				if file, ok := clone["path"].(string); ok {
					clone["path"] = rewrite(file)
				}
				out[index] = clone
			default:
				return nil, fmt.Errorf("entry %d is malformed", index)
			}
		}
		return out, nil
	case []string:
		out := make([]string, len(entries))
		for index, entry := range entries {
			out[index] = rewrite(entry)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("list is malformed")
	}
}

func secretEnvPathMatches(value, output string) bool {
	cleaned := strings.TrimPrefix(path.Clean(value), "./")
	output = strings.TrimPrefix(path.Clean(output), "./")
	if cleaned == output {
		return true
	}
	prefix := SecretGenerationDirectory + "/"
	return strings.HasPrefix(cleaned, prefix) && strings.HasSuffix(cleaned, "/"+output)
}

func setGenerationLabel(service map[string]any, generation string) error {
	raw, exists := service["labels"]
	if !exists {
		service["labels"] = map[string]any{"ob.secret-generation": generation}
		return nil
	}
	switch labels := raw.(type) {
	case map[string]any:
		labels["ob.secret-generation"] = generation
	case []any:
		out := make([]any, 0, len(labels)+1)
		for _, item := range labels {
			value, ok := item.(string)
			if !ok {
				return fmt.Errorf("list entry is malformed")
			}
			if !strings.HasPrefix(value, "ob.secret-generation=") {
				out = append(out, value)
			}
		}
		service["labels"] = append(out, "ob.secret-generation="+generation)
	default:
		return fmt.Errorf("mapping or list required")
	}
	return nil
}

func generationLabel(raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	switch labels := raw.(type) {
	case map[string]any:
		value, exists := labels["ob.secret-generation"]
		if !exists {
			return "", nil
		}
		generation, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("ob.secret-generation must be a string")
		}
		return generation, nil
	case []any:
		for _, item := range labels {
			value, ok := item.(string)
			if !ok {
				return "", fmt.Errorf("list entry is malformed")
			}
			if generation, found := strings.CutPrefix(value, "ob.secret-generation="); found {
				return generation, nil
			}
		}
		return "", nil
	default:
		return "", fmt.Errorf("mapping or list required")
	}
}
