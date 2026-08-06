package app

import (
	"os"
	"path/filepath"
	"testing"
)

const probeNoOverrides = `api_version: onebox.run/v1
app: demo
environments:
  prod:
    server: root@example.com
runtime:
  env_files: [missing.env]
workloads:
  web:
    role: application
    image: nginx:1
`

const probeWithOverrides = `api_version: onebox.run/v1
app: demo
environments:
  prod:
    server: root@example.com
    overrides:
      workloads:
        web:
          replicas: 2
runtime:
  env_files: [missing.env]
workloads:
  web:
    role: application
    image: nginx:1
    replicas: 1
`

func writeProbe(t *testing.T, body string) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "onebox.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeMissingEnvFileOnLoad(t *testing.T) {
	p := writeProbe(t, probeNoOverrides)
	spec, err := Load(p)
	t.Logf("Load err = %v", err)
	if err != nil {
		return
	}
	_, err = spec.Resolve("prod")
	t.Logf("Resolve(no overrides) err = %v", err)
}

func TestProbeMissingEnvFileWithOverrides(t *testing.T) {
	p := writeProbe(t, probeWithOverrides)
	spec, err := Load(p)
	t.Logf("Load err = %v", err)
	if err != nil {
		return
	}
	_, err = spec.Resolve("prod")
	t.Logf("Resolve(with overrides) err = %v", err)
}
