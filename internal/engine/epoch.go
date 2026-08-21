package engine

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// nextEpoch reads one durable fencing authority. Absence is the only state
// that means zero: an unreadable or malformed value must never reissue an epoch
// a stale runner may still hold.
func (e *Engine) nextEpoch(ctx context.Context, epochPath string) (int, error) {
	result, err := e.T.Run(ctx, epochProbeCmd(epochPath))
	if err != nil {
		return 0, err
	}
	switch result.ExitCode {
	case 0:
		// Parsed below.
	case app.ProbeAbsent:
		return 1, nil
	case app.ProbeUnreadable:
		return 0, fmt.Errorf("epoch file %s exists but cannot be read", epochPath)
	case app.ProbeNotRegular:
		return 0, fmt.Errorf("epoch file %s is not a regular file", epochPath)
	case app.ProbeUndetermined:
		return 0, fmt.Errorf("epoch file %s cannot be observed because an ancestor directory is not searchable", epochPath)
	case app.ProbeStatePathNotDirectory:
		return 0, fmt.Errorf("epoch file %s cannot be read because an ancestor path is not a directory", epochPath)
	default:
		return 0, fmt.Errorf("read epoch file %s failed (exit %d): %s", epochPath, result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	raw := strings.TrimSpace(result.Stdout)
	previous, err := strconv.Atoi(raw)
	if err != nil || previous < 0 || previous == int(^uint(0)>>1) {
		return 0, fmt.Errorf("epoch file %s contains invalid value %q", epochPath, raw)
	}
	return previous + 1, nil
}

// epochProbeCmd distinguishes a file that is genuinely absent from one hidden
// by permissions or replaced with another kind of filesystem object.
func epochProbeCmd(epochPath string) string {
	p := q(epochPath)
	return ": ob-epoch-probe; if [ ! -e " + p + " ] && [ ! -L " + p + " ]; then " +
		app.UndeterminedArm(epochPath) + "exit " + strconv.Itoa(app.ProbeAbsent) + "; fi; " +
		"if [ ! -f " + p + " ] || [ -L " + p + " ]; then exit " + strconv.Itoa(app.ProbeNotRegular) + "; fi; " +
		"if [ ! -r " + p + " ]; then exit " + strconv.Itoa(app.ProbeUnreadable) + "; fi; cat " + p
}

// atomicEpochWriteCmd writes beside the epoch and renames over it. A killed
// shell can leave a disposable temp file, never a truncated authority.
func atomicEpochWriteCmd(epochPath string, epoch int) string {
	template := epochPath + ".tmp.XXXXXX"
	return "set -eu; test -d " + q(path.Dir(epochPath)) + "; umask 077; tmp=$(mktemp " + q(template) + "); " +
		`trap 'rm -f "$tmp"' 0 1 2 15; printf '%s\n' ` + strconv.Itoa(epoch) + ` > "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" ` + q(epochPath) + `; trap - 0 1 2 15`
}
