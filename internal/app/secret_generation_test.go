package app

import (
	"strings"
	"testing"
)

func TestSecretGenerationValidatorIsStrict(t *testing.T) {
	valid := "sg-0123456789abcdef01234567"
	if !IsSecretGeneration(valid) {
		t.Fatalf("canonical generation %q was rejected", valid)
	}
	for _, invalid := range []string{
		"", "sg-0123456789abcdef0123456", "sg-0123456789abcdef012345678",
		"sg-0123456789ABCDEF01234567", "sg-0123456789abcdef0123456g",
		"../" + valid, valid + "/child", "generation-0123456789abcdef01234567",
	} {
		t.Run(invalid, func(t *testing.T) {
			if IsSecretGeneration(invalid) {
				t.Fatalf("invalid generation %q was accepted", invalid)
			}
			if _, err := ApplySecretGeneration([]byte("services: {}\n"), nil, invalid); err == nil {
				t.Fatalf("runtime rewrite accepted invalid generation %q", invalid)
			}
		})
	}
}

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

func TestApplySecretGenerationReplacesListFormLabel(t *testing.T) {
	const oldGeneration = "sg-111111111111111111111111"
	const newGeneration = "sg-222222222222222222222222"
	input := []byte(`services:
  web:
    env_file: [.ob-decrypted-sops-api.env]
    labels: [ob.app=shop, ob.secret-generation=` + oldGeneration + `]
`)
	graph := []SecretDeclaration{{OutputPath: ".ob-decrypted-sops-api.env", AffectedWorkloads: []string{"web"}}}
	output, err := ApplySecretGeneration(input, graph, newGeneration)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, oldGeneration) || strings.Count(text, "ob.secret-generation=") != 1 {
		t.Fatalf("list-form generation label was not replaced exactly once:\n%s", text)
	}
	selected, err := SecretGenerationFromCompose(output, []string{"web"})
	if err != nil || selected != newGeneration {
		t.Fatalf("selected generation = %q, err=%v", selected, err)
	}
}
