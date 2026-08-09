package main

import "testing"

// argv must survive the trip to the container.
//
// The command was assembled with strings.Join(args, " ") and the remote side
// runs it through `sh -c`, so the shell re-split it: `ob exec web -- sh -c
// 'echo one; echo two'` arrived as `sh -c echo one; echo two`, echo ran with no
// arguments, "one" became $0, and the first statement's output vanished.
//
// This asserts on execCommand's output. An earlier version of this test
// re-implemented the quoting in the test body and so passed with the bug fully
// restored — it proved shellquote.Quote was self-consistent and nothing else.
func TestExecCommandPreservesArgumentBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"a statement list stays one argument", []string{"sh", "-c", "echo one; echo two"}, `'sh' '-c' 'echo one; echo two'`},
		{"an argument with spaces", []string{"echo", "hello world"}, `'echo' 'hello world'`},
		{"every element is quoted, so none can be re-split", []string{"id", "-u"}, `'id' '-u'`},
		{"an embedded quote is escaped, not dropped", []string{"sh", "-c", `echo "it's here"`}, `'sh' '-c' 'echo "it'\''s here"'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := execCommand(tt.args); got != tt.want {
				t.Errorf("execCommand(%q)\n got: %s\nwant: %s", tt.args, got, tt.want)
			}
		})
	}
}
