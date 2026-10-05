// Package gitinfo reports the local checkout's revision for operation provenance.
package gitinfo

import (
	"context"
	"os/exec"
	"strings"
)

// Revision returns HEAD's short SHA, suffixed with +dirty when tracked or
// untracked files differ. Git-ignored files are excluded, as in git status;
// this is checkout provenance, not a digest of the staged release payload.
// If either HEAD or working-tree state is unavailable, it makes no claim.
func Revision(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	revision := strings.TrimSpace(string(out))
	status, err := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", dir,
		"status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none").Output()
	if err != nil {
		return ""
	}
	if len(status) > 0 {
		revision += "+dirty"
	}
	return revision
}
