package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

// The one unrecoverable window in a deploy is: checkpoint cleared, release
// serving, nothing journalled. finalize then refuses forever ("its journal
// records no successful activation") on a release that is healthy and live,
// and the refusal's guidance would roll it back. So the clear must come after
// the activation result reaches the journal, never before.
func TestActivationClearsItsCheckpointOnlyAfterJournallingSuccess(t *testing.T) {
	fake := &transport.Fake{}
	engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep, Environment: "production", ForceLock: true})
	// The deploy is expected to fail somewhere; what matters is the relative
	// order of the two writes whenever both occur.
	_ = engine.Deploy(context.Background(), engineTestDeployReleaseID, t.TempDir())

	clearedAt, journalledAt := -1, -1
	for i, command := range fake.Commands {
		if journalledAt < 0 && strings.Contains(command, "activation") && strings.Contains(command, "\"event\":\"result\"") && strings.Contains(command, "\"status\":\"ok\"") {
			journalledAt = i
		}
		if clearedAt < 0 && strings.Contains(command, "rm -f") && strings.Contains(command, "activation.json") {
			clearedAt = i
		}
	}
	if clearedAt < 0 {
		return // never got that far; nothing to order
	}
	if journalledAt < 0 {
		t.Fatalf("the activation checkpoint was cleared with no activation result in the journal (command %d)", clearedAt)
	}
	if clearedAt < journalledAt {
		t.Fatalf("checkpoint cleared at %d, before the activation was journalled at %d", clearedAt, journalledAt)
	}
}
