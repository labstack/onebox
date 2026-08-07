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
)

var protectionCredentialEntry = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

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
