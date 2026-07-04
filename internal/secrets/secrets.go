// Package secrets renders a SOPS-encrypted YAML file into a flat env file.
// Decryption is runner-side (`sops -d` — sops+age is the curated provider,
// design §03); the host only ever sees the rendered file at mode 600 inside
// a release dir.
package secrets

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvFileName is the rendered file's name inside a release dir; render
// injects it as an env_file on role services.
const EnvFileName = ".ob-secrets.env"

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Render decrypts the SOPS file and renders sorted KEY=VALUE lines.
// The encrypted file must be a flat map — nesting is an error, not a guess.
func Render(configDir, sopsFile string) ([]byte, error) {
	path := sopsFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, sopsFile)
	}
	out, err := exec.Command("sops", "-d", path).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("sops -d %s: %s", sopsFile, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("sops not available on this machine: %w", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("%s: decrypted content is not a YAML map: %w", sopsFile, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("%s: key %q is not a valid env var name", sopsFile, k)
		}
		switch v := m[k].(type) {
		case string, int, int64, float64, bool:
			fmt.Fprintf(&b, "%s=%v\n", k, v)
		default:
			return nil, fmt.Errorf("%s: key %q is nested — secrets must be a flat map", sopsFile, k)
		}
	}
	return []byte(b.String()), nil
}
