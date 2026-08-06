// Package secrets renders a SOPS-encrypted YAML file into a flat env file.
// Decryption is runner-side (`sops -d` — sops+age is the curated provider,
// package; the host only ever sees the rendered file at mode 600 inside
// a release dir.
package secrets

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"bytes"
	"gopkg.in/yaml.v3"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// EnvFileName is the rendered file's name inside a release dir; render
// injects it as an env_file on role services.
const EnvFileName = ".ob-secrets.env"

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Render decrypts the SOPS file and renders sorted KEY=VALUE lines.
// The encrypted file must be a flat map — nesting is an error, not a guess.
func Render(configDir, sopsFile string) ([]byte, error) {
	return RenderContext(context.Background(), configDir, sopsFile)
}

// RenderContext is Render with cancellation propagated to SOPS and any KMS or
// age plugin it invokes.
func RenderContext(ctx context.Context, configDir, sopsFile string) ([]byte, error) {
	path := sopsFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, sopsFile)
	}
	out, err := exec.CommandContext(ctx, "sops", "-d", path).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("sops -d %s: %s", sopsFile, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("sops not available on this machine: %w", err)
	}
	return renderDecrypted(sopsFile, out)
}

// renderDecrypted turns decrypted bytes into the environment file the
// container runtime reads, accepting either form the plaintext may take.
func renderDecrypted(sopsFile string, out []byte) ([]byte, error) {
	// YAML first, so every rule that applied to a YAML map still applies —
	// nested values and invalid key names are rejected exactly as before.
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err == nil && m != nil {
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

	// Otherwise the plaintext is already an environment file, which is the
	// shape the field's name invites: an author names a `.env`, writes
	// `KEY=value`, and encrypts it. SOPS decrypts by the file's own format, so
	// that comes back as dotenv, and demanding a YAML map rejected it with a
	// message naming nothing an author could act on.
	values, err := dotenv.Parse(bytes.NewReader(out))
	if err != nil || len(values) == 0 {
		return nil, fmt.Errorf("%s: decrypted content is neither an environment file nor a flat YAML map", sopsFile)
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("%s: key %q is not a valid env var name", sopsFile, k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, values[k])
	}
	return []byte(b.String()), nil
}
