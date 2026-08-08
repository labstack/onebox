package onebox

import (
	"errors"
	"sort"

	"github.com/labstack/onebox/internal/app"
)

// BindProtectionArtifacts projects only safe metadata from generated
// protection artifacts into a plan. Contents remain in their target-side
// files and every projected digest becomes part of the plan identity.
func BindProtectionArtifacts(plan *OperationPlan, generated app.ProtectionArtifactSet) error {
	if plan == nil {
		return errors.New("operation plan is nil")
	}
	bindings := make([]OperationArtifactBinding, len(generated.Artifacts))
	for index, artifact := range generated.Artifacts {
		bindings[index] = OperationArtifactBinding{
			Class: artifact.Class, Path: artifact.Path, Mode: artifact.Mode, Digest: artifact.Digest,
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Class < bindings[j].Class })
	plan.Artifacts = bindings
	plan.PlanDigest = ""
	return nil
}
