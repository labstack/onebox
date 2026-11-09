package onebox

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

func NewBackupReport(plan ExecutablePlan, reportedBy string, reportedAt time.Time, resources []MigrationBackupResourceEvidence, keyMaterial []MigrationBackupKeyMaterialEvidence) (BackupReport, error) {
	view, err := inspectExecutablePlan(plan)
	if err != nil {
		return BackupReport{}, fmt.Errorf("validate backup report plan: %w", err)
	}
	if view.migrationBackup == nil {
		return BackupReport{}, ErrBackupReportNotRequired
	}
	resources = append([]MigrationBackupResourceEvidence(nil), resources...)
	keyMaterial = append([]MigrationBackupKeyMaterialEvidence(nil), keyMaterial...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Resource.Component < resources[j].Resource.Component })
	sort.Slice(keyMaterial, func(i, j int) bool { return keyMaterial[i].Name < keyMaterial[j].Name })
	binding := view.operation.Binding
	report := BackupReport{
		SchemaVersion: BackupReportSchemaVersion,
		PlanDigest:    view.digest, OperationDigest: view.operation.PlanDigest,
		Application: binding.Application, Environment: binding.Environment, Server: binding.Server,
		ReportedBy: strings.TrimSpace(reportedBy), ReportedAt: reportedAt.UTC().Format(time.RFC3339Nano),
		Resources: resources, KeyMaterial: keyMaterial,
	}
	if err := report.Validate(); err != nil {
		return BackupReport{}, err
	}
	return report, nil
}

func LifecycleFailureMeaning(code string) (string, bool) {
	definition, ok := lifecycleFailureDefinitions[code]
	return definition.message, ok
}

func loadProjectAt(ctx context.Context, configPath, environment string, lenient bool, images app.Images) (*loadedProject, error) {
	return loadProjectRestricted(ctx, configPath, environment, lenient, images, false)
}

// loadProjectRestricted optionally narrows the image map to the workloads that
// cannot render without it.
//
// A plan's pinned images are digests resolved *after* the plan rendered, so
// feeding all of them back into the render would produce a different document
// than the one the plan bound — and the binding check would refuse its own
// plan. Only a build-sourced workload needs an image to render at all, and for
// that workload the plan's entry is the same reference the render already used.
func loadProjectRestricted(ctx context.Context, configPath, environment string, lenient bool, images app.Images, onlyBuilt bool) (*loadedProject, error) {
	lp, renderImages, err := resolveProjectRestricted(configPath, environment, images, onlyBuilt)
	if err != nil {
		return nil, err
	}
	if err := renderLoadedProject(ctx, lp, environment, lenient, renderImages); err != nil {
		return nil, err
	}
	return lp, nil
}
