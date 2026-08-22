package onebox

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeDurableArtifact atomically publishes a mode-0600 local authority
// artifact and syncs both its contents and the containing directory. A process
// crash after success therefore cannot leave only the rename in page cache.
func writeDurableArtifact(path, prefix string, encoded []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeWith := func(cause error) error {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("%w; close temporary artifact: %w", cause, closeErr)
		}
		return cause
	}
	if err := tmp.Chmod(0o600); err != nil {
		return closeWith(err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		return closeWith(err)
	}
	if err := tmp.Sync(); err != nil {
		return closeWith(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
