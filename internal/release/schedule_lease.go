package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

const scheduleLeaseFile = ".ob-schedule.lease"

// ActiveScheduleLeases returns release ids held by pinned scheduled jobs. The
// runner takes a shared kernel lock; cleanup probes for an exclusive lock while
// the application lock prevents a new runner from entering the release store.
func ActiveScheduleLeases(ctx context.Context, target transport.Transport, names app.Names) ([]string, error) {
	glob := q(PathsFor(names).Releases) + "/*/" + scheduleLeaseFile
	command := "for lease in " + glob + "; do " +
		"[ -e \"$lease\" ] || continue; " +
		"[ -f \"$lease\" ] && [ ! -L \"$lease\" ] || exit 74; " +
		"code=0; /usr/bin/flock --exclusive --nonblock --conflict-exit-code 75 \"$lease\" true || code=$?; " +
		"case $code in 0) ;; 75) dir=${lease%/" + scheduleLeaseFile + "}; printf '%s\\n' \"${dir##*/}\" ;; *) exit $code ;; esac; " +
		"done"
	result, err := target.Run(ctx, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("inspect scheduled-job release leases failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if !IsID(id) {
			return nil, fmt.Errorf("scheduled-job release lease reported invalid release id %q", id)
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
