// Package secrets renders a SOPS-encrypted file into the environment file a
// container reads. The plaintext may be an environment file already or a flat
// YAML map; both render to the same thing and a nested map is refused.
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

	"gopkg.in/yaml.v3"
)

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Render decrypts the SOPS file and renders sorted KEY=VALUE lines.
// A YAML plaintext must be a flat map — nesting is an error, not a guess.
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
	if err := yaml.Unmarshal(out, &m); err == nil && len(m) > 0 {
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
	// A `{}` payload decodes as a valid empty map, which used to render zero
	// bytes and report success — a rotation script that emptied the file would
	// have started the application with every credential unset. Empty is a
	// failure, not an environment.
	values, err := parseLiteralEnv(out)
	if err != nil {
		return nil, fmt.Errorf("%s: decrypted content is neither an environment file nor a flat YAML map: %w", sopsFile, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s: decrypted content declares no values", sopsFile)
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

// parseLiteralEnv reads decrypted plaintext as an environment file without
// expanding anything.
//
// A general dotenv parser substitutes `$VAR` and `${VAR}` while reading. That
// is right for a file an author wrote and wrong for a decrypted secret, which
// is bytes rather than a template: a bcrypt hash or a generated password
// containing `$` is silently truncated at the first one — `$2y$10$abc` becomes
// `$2y$10` — and the parser logs the fragment it could not resolve as a
// warning, putting part of the credential on stderr in a package whose rule is
// that content never travels, only hashes.
//
// So this reads literally. Comments, blank lines, an `export` prefix and
// surrounding quotes are handled because a file someone encrypted may carry
// them; nothing else is interpreted.
func parseLiteralEnv(in []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(string(in), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("not an environment file")
		}
		key = strings.TrimSpace(key)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out[key] = value
	}
	return out, nil
}
