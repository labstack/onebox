package main

import (
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/onebox"
)

// Every failure names a command the operator can actually run.
//
// safeResolvingCommand checks a remedy is shell-safe and starts with `ob `,
// which is a claim about its form, not its truth. Eighteen of thirty-five
// lifecycle codes passed that guard while naming verbs the CLI has never had —
// `ob backup inspect`, `ob protection enable`, `ob assurance status`. A code is
// read at the moment something is broken, so a remedy that exits with
// `unknown command` costs the operator a round trip and their trust in the
// rest of the page.
func TestEveryLifecycleRemedyNamesARealCommand(t *testing.T) {
	root := newRootCmd()
	for _, code := range onebox.LifecycleFailureCodes() {
		failure, err := onebox.NewLifecycleFailure(code)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if failure.Next == "" {
			continue
		}
		fields := strings.Fields(failure.Next)
		if fields[0] != "ob" {
			t.Errorf("%s: remedy %q does not start with ob", code, failure.Next)
			continue
		}
		var path []string
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "-") {
				break
			}
			path = append(path, field)
		}
		want := "ob " + strings.Join(path, " ")
		cmd, _, err := root.Find(path)
		if err != nil || cmd == nil || cmd.CommandPath() != want {
			t.Errorf("%s: remedy %q names %q, which is not a command", code, failure.Next, want)
		}
	}
}
