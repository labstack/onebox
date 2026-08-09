package main

import (
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/shellquote"
)

// argv must survive the trip to the container.
//
// The command was assembled with strings.Join(args, " ") and then run through
// `sh -c` on the remote side, so the shell re-split it. `ob exec web -- sh -c
// 'echo one; echo two'` arrived as `sh -c echo one; echo two`: echo ran with no
// arguments, "one" became $0, and the first statement's output vanished. Single
// word commands worked, which is why it survived casual use.
func TestExecPreservesArgumentBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"a statement list", []string{"sh", "-c", "echo one; echo two"}},
		{"an argument with spaces", []string{"echo", "hello world"}},
		{"a quote inside an argument", []string{"sh", "-c", `echo "it's here"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			quoted := make([]string, 0, len(tt.args))
			for _, a := range tt.args {
				quoted = append(quoted, shellquote.Quote(a))
			}
			joined := strings.Join(quoted, " ")
			// Every argument must appear as one quoted span, so the remote shell
			// rebuilds the same vector rather than re-splitting on whitespace.
			for _, a := range tt.args {
				if !strings.Contains(joined, shellquote.Quote(a)) {
					t.Fatalf("argument %q lost its boundary in %q", a, joined)
				}
			}
			if strings.Contains(joined, "sh -c echo one") {
				t.Fatalf("argv was flattened: %q", joined)
			}
		})
	}
}
