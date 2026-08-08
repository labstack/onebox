package onebox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/engine"
)

// SeedActiveVolume records the stable service volume for an installation that
// predates active-volume state. It observes only: no Docker volume is created,
// renamed, copied, labelled, or adopted.
func SeedActiveVolume(ctx context.Context, execution *engine.Engine, service, logicalName, operationID string, epoch int) (ActiveVolumeRecord, bool, error) {
	if execution == nil || execution.Spec == nil {
		return ActiveVolumeRecord{}, false, errors.New("active-volume seed requires an execution engine")
	}
	names := execution.Names()
	statePath := names.ActiveVolumeFile(service)
	stateResult, err := execution.T.Run(ctx,
		"if [ -f "+quote(statePath)+" ]; then cat "+quote(statePath)+"; elif [ -e "+quote(statePath)+" ]; then exit 2; else exit 3; fi",
	)
	if err != nil {
		return ActiveVolumeRecord{}, false, err
	}
	switch stateResult.ExitCode {
	case 0:
		record, err := DecodeActiveVolumeRecord([]byte(stateResult.Stdout))
		if err != nil {
			return ActiveVolumeRecord{}, false, err
		}
		if record.Application != names.App || record.Environment != execution.Spec.Env || record.Service != service || record.LogicalName != logicalName {
			return ActiveVolumeRecord{}, false, errors.New("existing active-volume state belongs to a different service identity")
		}
		return record, false, nil
	case 3:
		// Expected migration path; prove the stable volume exists and is owned.
	case 2:
		return ActiveVolumeRecord{}, false, errors.New("active-volume state path exists but is not a regular file")
	default:
		return ActiveVolumeRecord{}, false, errors.New("inspect active-volume state failed")
	}

	stableVolume := names.ServiceVolume(service, logicalName)
	ownerResult, err := execution.T.Run(ctx,
		"docker volume inspect --format '{{index .Labels \"com.docker.compose.project\"}}' "+quote(stableVolume),
	)
	if err != nil {
		return ActiveVolumeRecord{}, false, err
	}
	if ownerResult.ExitCode != 0 {
		return ActiveVolumeRecord{}, false, fmt.Errorf("active-volume seed requires existing stable volume %s; no volume was created", stableVolume)
	}
	expectedOwner := names.ServiceProject(service)
	owner := strings.TrimSpace(ownerResult.Stdout)
	if owner != expectedOwner {
		return ActiveVolumeRecord{}, false, fmt.Errorf("active-volume seed refuses volume %s owned by %s; expected %s", stableVolume, safeObservedOwner(owner), expectedOwner)
	}

	record, err := NewActiveVolumeRecord(names.App, execution.Spec.Env, service, logicalName, stableVolume, operationID, epoch, nil)
	if err != nil {
		return ActiveVolumeRecord{}, false, err
	}
	encoded, err := EncodeActiveVolumeRecord(record)
	if err != nil {
		return ActiveVolumeRecord{}, false, err
	}
	temporary := statePath + ".tmp"
	write := "mkdir -p " + quote(names.AppDir()+"/protection/state") +
		" && umask 077 && printf %s " + quote(string(encoded)) + " > " + quote(temporary) +
		" && chmod 600 " + quote(temporary) + " && mv -f " + quote(temporary) + " " + quote(statePath)
	result, err := execution.ProtectionMutate(ctx, service, write)
	if err != nil {
		return ActiveVolumeRecord{}, false, err
	}
	if result.ExitCode != 0 {
		return ActiveVolumeRecord{}, false, errors.New("write seeded active-volume state failed")
	}
	return record, true, nil
}

func safeObservedOwner(owner string) string {
	if safeLifecycleMetadata(owner) {
		return owner
	}
	return "an unowned or foreign resource"
}
