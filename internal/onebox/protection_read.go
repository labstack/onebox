package onebox

import (
	"context"
	"fmt"

	"github.com/labstack/onebox/internal/engine"
)

// ProtectionStatus reads what a protected service's repository can actually
// recover.
//
// It takes no lock and mutates nothing, which is deliberate: the question "is
// this database recoverable" must be answerable while a deploy is in flight,
// and an operator who has to wait for a lock to find out is an operator who
// will stop asking.
func (s *Service) ProtectionStatus(ctx context.Context, service string) (engine.ProtectionStatus, error) {
	lp, err := s.loadProject(ctx, true)
	if err != nil {
		return engine.ProtectionStatus{}, fmt.Errorf("load project: %w", err)
	}
	if err := ensureEnvironment(lp.resolved, s.environment); err != nil {
		return engine.ProtectionStatus{}, err
	}
	e, cleanup, _, err := s.engine(ctx, lp, s.environment)
	if err != nil {
		return engine.ProtectionStatus{}, fmt.Errorf("connect target: %w", err)
	}
	defer cleanup()
	return e.ProtectionStatusFor(ctx, service)
}
