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

var backupCredentialEntry = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// InstallBackupCredentialFile moves already-resolved credential material
// through a private upload into its target-side mode-0600 file. Secret bytes
// are never interpolated into a command, journal, result, or error.
func (e *Engine) InstallBackupCredentialFile(ctx context.Context, service, target string, requiredEntries []string, plaintext []byte) (string, error) {
	if !backupIdentity.MatchString(service) || !backupIdentity.MatchString(target) {
		return "", errors.New("backup credential service and target identities are invalid")
	}
	entries, err := backupCredentialEntries(plaintext)
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
		if !backupCredentialEntry.MatchString(entry) {
			return "", errors.New("backup credential contract contains an invalid slot")
		}
		if !entries[entry] {
			return "", fmt.Errorf("backup credential file is missing required entry %s", entry)
		}
	}

	localStaging, err := os.MkdirTemp("", "ob-backup-credentials-")
	if err != nil {
		return "", errors.New("create private backup credential staging")
	}
	defer os.RemoveAll(localStaging)
	const stagedName = "credentials.env"
	if err := os.WriteFile(filepath.Join(localStaging, stagedName), plaintext, 0o600); err != nil {
		return "", errors.New("write private backup credential staging")
	}

	names := e.names()
	destination := names.BackupCredentialFile(service, target)
	tokenBytes := sha256.Sum256([]byte(e.backupFenceVals[service] + "\x00" + target))
	token := hex.EncodeToString(tokenBytes[:])[:16]
	remoteStaging := names.AppDir() + "/backup/.credential-staging-" + service + "-" + token
	if err := e.T.Upload(ctx, localStaging, remoteStaging); err != nil {
		return "", errors.New("upload private backup credential staging")
	}
	// Cleanup cannot use BackupMutate: a failed/fenced install is exactly
	// when that guard may refuse the command. The paths are deterministic,
	// narrowly scoped staging artifacts, and best-effort removal must run even
	// after cancellation so plaintext is not stranded on the host.
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.T.Run(cleanupContext, "rm -rf "+q(remoteStaging)+"; rm -f "+q(destination+".tmp"))
	}()
	install := "mkdir -p " + q(names.BackupSecretDir()) +
		" && chmod 700 " + q(names.BackupSecretDir()) +
		" && cp " + q(remoteStaging+"/"+stagedName) + " " + q(destination+".tmp") +
		" && chmod 600 " + q(destination+".tmp") +
		" && mv -f " + q(destination+".tmp") + " " + q(destination) +
		" && rm -rf " + q(remoteStaging)
	result, err := e.BackupMutate(ctx, service, install)
	if err != nil {
		return "", errors.New("install target-side backup credential file")
	}
	if result.ExitCode != 0 {
		return "", errors.New("install target-side backup credential file failed")
	}
	return destination, nil
}

// MigrateBackupCredentialFiles copies the pre-2026.8.6 ambiguous spelling to
// the escaped path before Compose reads it. The old file remains for rollback
// to an older binary; disablement removes both spellings.
func (e *Engine) MigrateBackupCredentialFiles(ctx context.Context) error {
	names := e.names()
	type migration struct {
		current string
		legacy  string
	}
	var migrations []migration
	legacyUsers := map[string][]string{}
	for _, service := range e.Spec.ServiceNames() {
		if !e.Spec.ServiceIsProtected(service) {
			continue
		}
		projection, err := e.Spec.EffectiveBackupProjection(service)
		if err != nil {
			return err
		}
		files := names.BackupCredentialFiles(service, projection.Policy.Target)
		if len(files) == 1 {
			continue
		}
		migrations = append(migrations, migration{current: files[0], legacy: files[1]})
		legacyUsers[files[1]] = append(legacyUsers[files[1]], files[0])
	}
	for _, migration := range migrations {
		users := legacyUsers[migration.legacy]
		if len(users) > 1 {
			checks := make([]string, 0, len(users))
			for _, current := range users {
				checks = append(checks, "[ -f "+q(current)+" ]")
			}
			res, err := e.T.Run(ctx, "if [ -f "+q(migration.legacy)+" ]; then "+strings.Join(checks, " && ")+"; fi")
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("legacy backup credential path %s belongs to more than one service/target pair; re-enable each affected backup so Onebox can establish the escaped credential paths", migration.legacy)
			}
			continue
		}
		command := "if [ ! -f " + q(migration.current) + " ] && [ -f " + q(migration.legacy) + " ]; then " +
			"install -m 600 " + q(migration.legacy) + " " + q(migration.current) + "; fi"
		res, err := e.mutate(ctx, command)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("cannot migrate backup credential file %s: %s", migration.legacy, strings.TrimSpace(res.Stderr))
		}
	}
	return nil
}

func backupCredentialEntries(plaintext []byte) (map[string]bool, error) {
	entries := make(map[string]bool)
	for index, line := range strings.Split(string(plaintext), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		entry, _, ok := strings.Cut(trimmed, "=")
		entry = strings.TrimSpace(entry)
		if !ok || !backupCredentialEntry.MatchString(entry) {
			return nil, fmt.Errorf("backup credential file has an invalid entry at line %d", index+1)
		}
		if entries[entry] {
			return nil, fmt.Errorf("backup credential file repeats entry %s", entry)
		}
		entries[entry] = true
	}
	if len(entries) == 0 {
		return nil, errors.New("backup credential file has no entries")
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
