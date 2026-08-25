// Package secrets renders a SOPS-encrypted file into the environment file a
// container reads. The plaintext may be an environment file already or a flat
// YAML map; both render to the same thing and a nested map is refused.
// Decryption is runner-side (`sops -d` — sops+age is the curated provider,
// package; the host only ever sees the rendered file at mode 600 inside
// a release dir.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("sops -d %s: %s", sopsFile, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("sops not available on this machine: %w", err)
	}
	return renderDecrypted(sopsFile, out)
}

// RenderBytesContext decrypts one immutable source snapshot. The temporary
// file preserves the original basename so SOPS can infer its input format;
// callers can fingerprint exactly the ciphertext bytes that produced the
// returned runtime payload without a second, racy read of the source path.
func RenderBytesContext(ctx context.Context, sourceName string, encrypted []byte) ([]byte, error) {
	directory, err := os.MkdirTemp("", "ob-sops-snapshot")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	name := filepath.Base(sourceName)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return nil, errors.New("encrypted source name is invalid")
	}
	if err := os.WriteFile(filepath.Join(directory, name), encrypted, 0o600); err != nil {
		return nil, err
	}
	return RenderContext(ctx, directory, name)
}

// renderDecrypted turns decrypted bytes into the environment file the
// container runtime reads, accepting either form the plaintext may take.
func renderDecrypted(sopsFile string, out []byte) ([]byte, error) {
	// An environment file is passed through unchanged. Parsing and re-emitting
	// it corrupts values in ways nothing downstream can detect: quotes are
	// stripped here and the container runtime then reads `KEY="a #b"` as `a`,
	// and padding inside quotes is lost the same way. The bytes were already
	// the format the runtime reads, so the safe transformation is none.
	if keys, ok := environmentFileKeys(out); ok {
		for _, k := range keys {
			if !keyRe.MatchString(k) {
				return nil, fmt.Errorf("%s: key %q is not a valid env var name", sopsFile, k)
			}
		}
		return out, nil
	}

	// Otherwise a flat YAML map, rendered into that format.
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil || len(m) == 0 {
		return nil, fmt.Errorf("%s: decrypted content is neither an environment file nor a flat YAML map", sopsFile)
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
		case string:
			if strings.IndexByte(v, 0) >= 0 {
				return nil, fmt.Errorf("%s: key %q contains a NUL byte", sopsFile, k)
			}
			fmt.Fprintf(&b, "%s=%s\n", k, quoteEnvString(v))
		case int, int64, float64, bool:
			fmt.Fprintf(&b, "%s=%v\n", k, v)
		default:
			return nil, fmt.Errorf("%s: key %q is nested — secrets must be a flat map", sopsFile, k)
		}
	}
	return []byte(b.String()), nil
}

// ProjectEnvironment emits only mapped keys from an already-rendered dotenv
// payload. It preserves each raw right-hand side byte-for-byte, avoiding a
// parse/re-encode cycle that could expand dollars or alter quoting in secrets.
func ProjectEnvironment(source []byte, entries map[string]string) ([]byte, error) {
	if len(entries) == 0 {
		return nil, errors.New("external connection projection has no entries")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(source), "\n") {
		line = strings.TrimSuffix(line, "\r")
		candidate := strings.TrimLeft(line, " \t")
		if candidate == "" || strings.HasPrefix(candidate, "#") {
			continue
		}
		candidate = strings.TrimPrefix(candidate, "export ")
		key, value, ok := strings.Cut(candidate, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyRe.MatchString(key) {
			return nil, errors.New("trusted external connection is not a valid environment file")
		}
		values[key] = value
	}
	destinations := make([]string, 0, len(entries))
	for destination := range entries {
		if !keyRe.MatchString(destination) {
			return nil, fmt.Errorf("external connection destination %q is not a valid env var name", destination)
		}
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	var projected strings.Builder
	for _, destination := range destinations {
		sourceKey := entries[destination]
		value, ok := values[sourceKey]
		if !ok {
			return nil, fmt.Errorf("trusted external connection is missing required entry %s", sourceKey)
		}
		fmt.Fprintf(&projected, "%s=%s\n", destination, value)
	}
	return []byte(projected.String()), nil
}

// quoteEnvString emits a Compose dotenv value without changing its bytes when
// Compose parses it. Quoting protects whitespace and #; escaping also prevents
// interpolation of secret values containing $.
func quoteEnvString(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `$$`,
		"\a", `\a`,
		"\b", `\b`,
		"\f", `\f`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
		"\v", `\v`,
	)
	return `"` + replacer.Replace(value) + `"`
}

// environmentFileKeys reports the keys of a dotenv payload, and whether the
// payload is one at all.
//
// This decides which shape the plaintext is, and it has to decide before YAML
// gets a chance: `CONFIG={"a": 1}` and `GREETING=hello: world` are ordinary
// environment lines that YAML also accepts as a one-key mapping, and letting
// YAML win made a service-account blob — one of the most common things anyone
// encrypts — fail naming a key the author never wrote.
func environmentFileKeys(in []byte) ([]string, bool) {
	var keys []string
	for _, line := range strings.Split(string(in), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false
		}
		keys = append(keys, strings.TrimSpace(key))
	}
	return keys, len(keys) > 0
}
