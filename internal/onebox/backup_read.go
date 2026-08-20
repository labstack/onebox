package onebox

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/engine"
)

// BackupStatus reads what a protected service's repository can actually
// recover.
//
// It mutates nothing and takes no operation lock, which is deliberate: the
// question "is this database recoverable" must be answerable while a deploy is
// in flight, and an operator who has to wait to find out is one who stops
// asking. The wal-g listing underneath takes a *shared* repository lock with a
// short timeout, so it reads consistently without queueing behind a backup.
func (s *Service) BackupStatus(ctx context.Context, service string) (engine.BackupStatus, error) {
	lp, err := s.loadProject(ctx, true)
	if err != nil {
		return engine.BackupStatus{}, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return engine.BackupStatus{}, err
	}
	e, cleanup, _, err := s.engine(ctx, lp, s.environment)
	if err != nil {
		return engine.BackupStatus{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	return e.BackupStatusFor(ctx, service)
}
