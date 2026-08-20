package onebox

import (
	"testing"

	"github.com/labstack/onebox/internal/app"
)

func TestOnlyARenamedTargetRetiresItsCredentialFile(t *testing.T) {
	next := app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "offsite"}}
	sameName := &app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "offsite"}}
	otherName := &app.BackupEffectiveProjection{Policy: app.BackupPolicy{Target: "coldline"}}

	if retiresCredentialFile(sameName, next) {
		t.Fatal("editing a target in place retired the credential file this run installs")
	}
	if !retiresCredentialFile(otherName, next) {
		t.Fatal("moving to a differently named target left its credential file behind")
	}
	if retiresCredentialFile(nil, next) {
		t.Fatal("a first enablement retired a credential file that never existed")
	}
}
