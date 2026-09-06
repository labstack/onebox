package durable

import (
	"os"
	"os/exec"
	"testing"
)

func TestHostCheckpointProtocol(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("durable runner tests require python3")
	}
	cmd := exec.CommandContext(t.Context(), python, "-m", "unittest", "-v", "runner_test.py")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host checkpoint tests: %v\n%s", err, output)
	}
}
