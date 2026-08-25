package engine

import (
	"context"
	"fmt"
)

// retireRemovedWorkloads drains and removes long-running workloads that were
// declared by the predecessor but are absent from the release now serving.
//
// This is deliberately post-activation work. A workload rename is represented
// by Compose as one removed service and one added service; removing Compose
// orphans during the release step would stop the old service before the new one
// had verified. The post-activation journal makes this cleanup resumable, and
// direct container retirement preserves the predecessor's drain policy without
// removing its volumes.
func (e *Engine) retireRemovedWorkloads(ctx context.Context, predecessor string) error {
	if predecessor == "" {
		return nil
	}

	previous, err := e.engineFromReleaseSnapshotFor(ctx, predecessor, "retired-workload cleanup")
	if err != nil {
		return err
	}

	for _, name := range sortedNames(previous.Spec.Workloads) {
		old := previous.Spec.Workloads[name]
		if old.IsJob() {
			continue
		}
		if current, exists := e.Spec.Workloads[name]; exists && !current.IsJob() {
			continue
		}

		ids, err := previous.containerIDs(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect retired workload %s: %w", name, err)
		}
		_, pollEvery := old.ReadyTiming()
		for _, id := range ids {
			e.logf("retire removed workload %s (%s)", name, id[:min(12, len(id))])
			if err := previous.retireContainer(ctx, old, id, pollEvery); err != nil {
				return fmt.Errorf("retire removed workload %s: %w", name, err)
			}
		}
	}
	return nil
}
