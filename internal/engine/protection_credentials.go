package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var protectionCredentialEntry = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// InstallProtectionCredentialFile moves already-resolved credential material
// through a private upload into its target-side mode-0600 file. Secret bytes
// are never interpolated into a command, journal, result, or error.
func (e *Engine) InstallProtectionCredentialFile(ctx context.Context, service, target string, requiredEntries []string, plaintext []byte) (string, error) {
	if !protectionIdentity.MatchString(service) || !protectionIdentity.MatchString(target) {
		return "", errors.New("protection credential service and target identities are invalid")
	}
	entries, err := protectionCredentialEntries(plaintext)
	if err != nil {
		return "", err
	}
	// Normalised before it is written. The decrypted file may use `export NAME=`
	// and quoted values — both ordinary in a shell-sourced dotenv, and both
	// accepted by Compose's env_file parser. `docker run --env-file` accepts
	// neither: it would create a variable literally named "export NAME" and keep
	// the quotes as part of the value, so a recovery container would start with
	// no credentials at all. Writing one form means every consumer reads the
	// same thing.
	plaintext = normalizeCredentialFile(plaintext)
	requiredEntries = append([]string(nil), requiredEntries...)
	sort.Strings(requiredEntries)
	for _, entry := range requiredEntries {
		if !protectionCredentialEntry.MatchString(entry) {
			return "", errors.New("protection credential contract contains an invalid slot")
		}
		if !entries[entry] {
			return "", fmt.Errorf("protection credential file is missing required entry %s", entry)
		}
	}

	localStaging, err := os.MkdirTemp("", "ob-protection-credentials-")
	if err != nil {
		return "", errors.New("create private protection credential staging")
	}
	defer os.RemoveAll(localStaging)
	const stagedName = "credentials.env"
	if err := os.WriteFile(filepath.Join(localStaging, stagedName), plaintext, 0o600); err != nil {
		return "", errors.New("write private protection credential staging")
	}

	names := e.names()
	destination := names.ProtectionCredentialFile(service, target)
	tokenBytes := sha256.Sum256([]byte(e.protectionFenceVals[service] + "\x00" + target))
	token := hex.EncodeToString(tokenBytes[:])[:16]
	remoteStaging := names.AppDir() + "/protection/.credential-staging-" + service + "-" + token
	if err := e.T.Upload(ctx, localStaging, remoteStaging); err != nil {
		return "", errors.New("upload private protection credential staging")
	}
	// Cleanup cannot use ProtectionMutate: a failed/fenced install is exactly
	// when that guard may refuse the command. The paths are deterministic,
	// narrowly scoped staging artifacts, and best-effort removal must run even
	// after cancellation so plaintext is not stranded on the host.
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.T.Run(cleanupContext, "rm -rf "+q(remoteStaging)+"; rm -f "+q(destination+".tmp"))
	}()
	install := "mkdir -p " + q(names.ProtectionSecretDir()) +
		" && chmod 700 " + q(names.ProtectionSecretDir()) +
		" && cp " + q(remoteStaging+"/"+stagedName) + " " + q(destination+".tmp") +
		" && chmod 600 " + q(destination+".tmp") +
		" && mv -f " + q(destination+".tmp") + " " + q(destination) +
		" && rm -rf " + q(remoteStaging)
	result, err := e.ProtectionMutate(ctx, service, install)
	if err != nil {
		return "", errors.New("install target-side protection credential file")
	}
	if result.ExitCode != 0 {
		return "", errors.New("install target-side protection credential file failed")
	}
	return destination, nil
}

func protectionCredentialEntries(plaintext []byte) (map[string]bool, error) {
	entries := make(map[string]bool)
	for index, line := range strings.Split(string(plaintext), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		entry, _, ok := strings.Cut(trimmed, "=")
		entry = strings.TrimSpace(entry)
		if !ok || !protectionCredentialEntry.MatchString(entry) {
			return nil, fmt.Errorf("protection credential file has an invalid entry at line %d", index+1)
		}
		if entries[entry] {
			return nil, fmt.Errorf("protection credential file repeats entry %s", entry)
		}
		entries[entry] = true
	}
	if len(entries) == 0 {
		return nil, errors.New("protection credential file has no entries")
	}
	return entries, nil
}

// normalizeCredentialFile rewrites decrypted credential material into the one
// form every consumer parses: NAME=value, no export prefix, no surrounding
// quotes, comments and blank lines dropped.
func normalizeCredentialFile(plaintext []byte) []byte {
	var out strings.Builder
	for _, line := range strings.Split(string(plaintext), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out.WriteString(strings.TrimSpace(name) + "=" + value + "\n")
	}
	return []byte(out.String())
}
