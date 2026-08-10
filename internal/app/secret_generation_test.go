package app

import (
	"strings"
	"testing"
)

func TestApplySecretGenerationChangesOnlyAffectedSecretBindings(t *testing.T) {
	input := []byte(`services:
  web:
    env_file: [plain.env, .ob-decrypted-sops-api.env, .ob-service-postgres.env]
    labels: {ob.app: shop}
  worker:
    env_file: [.ob-decrypted-sops-worker.env]
    labels: {ob.app: shop}
`)
	graph := []SecretDeclaration{
		{OutputPath: ".ob-decrypted-sops-api.env", AffectedWorkloads: []string{"web"}},
		{OutputPath: ".ob-decrypted-sops-worker.env", AffectedWorkloads: []string{"worker"}},
	}
	generation := "sg-111111111111111111111111"
	output, err := ApplySecretGeneration(input, graph, generation)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, secret := range []string{".ob-decrypted-sops-api.env", ".ob-decrypted-sops-worker.env"} {
		if !strings.Contains(text, SecretGenerationPath(generation, secret)) {
			t.Fatalf("runtime does not select generation path for %s:\n%s", secret, text)
		}
	}
	for _, unchanged := range []string{"plain.env", ".ob-service-postgres.env"} {
		if !strings.Contains(text, unchanged) {
			t.Fatalf("non-secret binding %s changed:\n%s", unchanged, text)
		}
	}
	selected, err := SecretGenerationFromCompose(output, []string{"web", "worker"})
	if err != nil || selected != generation {
		t.Fatalf("selected generation = %q, err=%v", selected, err)
	}
}

func TestSecretGenerationFromComposeRefusesPartialOrMixedState(t *testing.T) {
	for name, runtime := range map[string]string{
		"partial": `services:
  web: {labels: {ob.secret-generation: sg-111111111111111111111111}}
  worker: {labels: {ob.app: shop}}
`,
		"mixed": `services:
  web: {labels: {ob.secret-generation: sg-111111111111111111111111}}
  worker: {labels: {ob.secret-generation: sg-222222222222222222222222}}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SecretGenerationFromCompose([]byte(runtime), []string{"web", "worker"}); err == nil {
				t.Fatal("divergent runtime generation was accepted")
			}
		})
	}
}
