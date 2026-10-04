package release

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

// RequireNoLegacyJobExecutions prevents the retired checkpoint store from
// losing its release references or having its runners replaced during upgrade.
// It deliberately neither interprets nor deletes the old records.
func RequireNoLegacyJobExecutions(ctx context.Context, target transport.Transport, names app.Names) error {
	parent := names.AppDir() + "/schedule"
	store := parent + "/executions"
	command := "if [ -d " + q(parent) + " ] && { [ ! -r " + q(parent) + " ] || [ ! -x " + q(parent) + " ]; }; then echo 'cannot inspect legacy job execution store' >&2; exit 1; fi; " +
		"if [ -e " + q(store) + " ] || [ -L " + q(store) + " ]; then exit 78; fi"
	result, err := target.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("check legacy job executions: %w", err)
	}
	if result.ExitCode == 78 {
		return fmt.Errorf("legacy job execution store %s exists: use the previous Onebox binary to pause its timers and finish or abandon executions, verify no job is running, then archive the store outside the application directory before upgrading; see the schedule-a-job guide", store)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("check legacy job executions (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}
