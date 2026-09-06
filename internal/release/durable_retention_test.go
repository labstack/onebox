package release

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestDurableRetentionReferencesProtectCollectibleRelease(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreferenced", true: "resumable"}[pinned], func(t *testing.T) {
			names := app.Names{App: "sample", BasePath: app.DefaultBasePath}
			old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			previous, current := "20260101-000000-previous", "20260102-000000-current"
			target := &transport.Fake{}
			writeRetentionManifest(t, target, names, retentionManifest(t, previous, KindApplication, StateSuperseded, "", old))
			writeRetentionManifest(t, target, names, retentionManifest(t, current, KindApplication, StateServing, "", old))
			base := target.Dynamic
			readPins := false
			target.Dynamic = func(command string) (transport.Result, bool) {
				switch {
				case strings.Contains(command, " pins "):
					readPins = true
					if pinned {
						return transport.Result{Stdout: previous + "\n"}, true
					}
					return transport.Result{}, true
				case strings.Contains(command, "ls -1A"):
					return transport.Result{Stdout: previous + "\n" + current + "\n"}, true
				case strings.Contains(command, "readlink"):
					return transport.Result{Stdout: "releases/" + current + "\n"}, true
				}
				if base != nil {
					return base(command)
				}
				return transport.Result{}, false
			}
			decision, err := RetentionCandidates(context.Background(), target, names, DefaultRetentionPolicy(1, old.AddDate(0, 8, 0)))
			if err != nil {
				t.Fatal(err)
			}
			if !readPins {
				t.Fatal("retention did not consult durable references")
			}
			if slices.Contains(decision.Victims, previous) == pinned {
				t.Fatalf("pinned=%t: unexpected victims %v", pinned, decision.Victims)
			}
			if pinned && !slices.Contains(decision.Preserve, previous) {
				t.Fatalf("durable release missing from preserved set: %v", decision.Preserve)
			}
		})
	}
}

func TestDurableRetentionUnusableEvidenceRefusesCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result transport.Result
	}{
		{"unreadable", transport.Result{ExitCode: 1, Stderr: "permission denied"}},
		{"corrupt checkpoint", transport.Result{ExitCode: 1, Stderr: "unsupported or corrupt execution record"}},
		{"missing helper", transport.Result{ExitCode: 127}},
		{"invalid reference", transport.Result{Stdout: "../../another-application\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.Contains(command, "ls -1A") {
					return transport.Result{Stdout: "20260101-000000-old\n"}, true
				}
				if strings.Contains(command, " pins ") {
					return tc.result, true
				}
				return transport.Result{}, false
			}}
			decision, err := RetentionCandidates(context.Background(), target, app.Names{App: "sample", BasePath: app.DefaultBasePath}, DefaultRetentionPolicy(1, time.Now()))
			var evidenceErr *RetentionEvidenceError
			if !errors.As(err, &evidenceErr) {
				t.Fatalf("expected retention evidence refusal, got %v", err)
			}
			if len(decision.Victims) != 0 {
				t.Fatalf("unusable evidence returned deletion candidates: %v", decision.Victims)
			}
			if tc.result.ExitCode != 0 && (!strings.Contains(err.Error(), fmt.Sprintf("exit %d", tc.result.ExitCode)) || !strings.Contains(err.Error(), tc.result.Stderr)) {
				t.Fatalf("retention refusal lost helper diagnostic: %v", err)
			}
		})
	}
}
