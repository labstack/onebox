// Package shellquote quotes literal arguments for POSIX-compatible shells.
package shellquote

import "strings"

// Quote wraps value in single quotes, escaping embedded single quotes by
// closing the quoted string, writing an escaped quote, and reopening it.
func Quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
