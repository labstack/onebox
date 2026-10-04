package release

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func TestLegacyJobExecutionGuardOnDisk(t *testing.T) {
	for _, kind := range []string{"absent", "directory", "file", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			names := app.Names{App: "sample", BasePath: t.TempDir()}
			parent := names.AppDir() + "/schedule"
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			store := parent + "/executions"
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(store, 0o700)
			case "file":
				err = os.WriteFile(store, []byte("legacy checkpoint"), 0o600)
			case "dangling symlink":
				err = os.Symlink(store+".missing", store)
			}
			if err != nil {
				t.Fatal(err)
			}
			target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				cmd := exec.CommandContext(t.Context(), "sh", "-c", command)
				output, err := cmd.CombinedOutput()
				if err == nil {
					return transport.Result{}, true
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				return transport.Result{ExitCode: exit.ExitCode(), Stderr: string(output)}, true
			}}
			err = RequireNoLegacyJobExecutions(t.Context(), target, names)
			if kind == "absent" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "archive") {
					t.Fatalf("legacy store accepted: %v", err)
				}
				if _, err := os.Lstat(store); err != nil {
					t.Fatalf("guard changed legacy store: %v", err)
				}
			}
		})
	}
}

func TestLegacyJobExecutionsRefuseRetentionWithoutCandidates(t *testing.T) {
	for _, result := range []transport.Result{{ExitCode: 78}, {ExitCode: 1, Stderr: "permission denied"}} {
		target := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
			if strings.Contains(command, "/schedule/executions") {
				return result, true
			}
			t.Fatalf("retention continued after unusable legacy evidence: %s", command)
			return transport.Result{}, false
		}}
		decision, err := RetentionCandidates(context.Background(), target, app.Names{App: "sample", BasePath: app.DefaultBasePath}, DefaultRetentionPolicy(1, time.Now()))
		var evidenceErr *RetentionEvidenceError
		if !errors.As(err, &evidenceErr) || len(decision.Victims) != 0 {
			t.Fatalf("retention must refuse without candidates: %+v, %v", decision, err)
		}
		if result.Stderr != "" && !strings.Contains(err.Error(), result.Stderr) {
			t.Fatalf("lost host diagnostic: %v", err)
		}
	}
}

func TestLegacyJobExecutionGuardPropagatesTransportFailure(t *testing.T) {
	want := errors.New("connection lost")
	target := &transport.Fake{Err: func(string) error { return want }}
	if err := RequireNoLegacyJobExecutions(t.Context(), target, app.Names{}); !errors.Is(err, want) {
		t.Fatalf("lost transport error: %v", err)
	}
}
