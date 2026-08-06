package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestRemoveSchedulesRejectsFailedDisable(t *testing.T) {
	f := &transport.Fake{Dynamic: func(cmd string) (transport.Result, bool) {
		switch {
		case strings.Contains(cmd, "list-unit-files"):
			return transport.Result{Stdout: "ob-sample-nightly.timer\n"}, true
		case strings.Contains(cmd, "systemctl disable --now"):
			return transport.Result{ExitCode: 5, Stderr: "unit is busy"}, true
		}
		return transport.Result{}, false
	}}
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.RemoveSchedules(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove schedule ob-sample-nightly failed (exit 5): unit is busy") {
		t.Fatalf("remove schedules error = %v", err)
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "timer >/dev/null 2>&1 && rm -f") {
		t.Fatalf("disable failure must prevent unit-file removal:\n%s", seq)
	}
	if strings.Contains(seq, "systemctl daemon-reload") {
		t.Fatalf("schedule removal continued after disable failure:\n%s", seq)
	}
}
