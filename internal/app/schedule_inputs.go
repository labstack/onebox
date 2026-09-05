package app

import (
	"fmt"
	"regexp"
	"strings"
)

// ReservedInputPrefix is Onebox's environment namespace. Declared inputs may
// not enter it, and the manual-run metadata line the runner reads lives there.
const ReservedInputPrefix = "ONEBOX_"

// maxInputValueBytes bounds a value the way the exec reason is bounded: it is
// public operational metadata that travels through a file, a command line and
// a journal record, and none of those want a paragraph.
const maxInputValueBytes = 256

// gInputName is deliberately narrower than gEnvName: inputs become
// environment variables, and the upper-case convention keeps them visibly
// distinct from whatever the image sets for itself.
var gInputName = grammar{"input name", regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`),
	"upper-case letters, digits and underscores, starting with a letter"}

// InputValueAllowed is the charset that lets the runner pass a value through
// to the container and into the run record without escaping anything. It is
// checked regardless of the declared pattern, because a pattern is the
// project's promise about meaning, not the runner's guarantee about shell.
func InputValueAllowed(v string) bool {
	if len(v) > maxInputValueBytes {
		return false
	}
	for _, r := range v {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// accepts reports whether v satisfies this input. A pattern matches the whole
// value: an author who writes `[0-9]+` means a number, not a value that
// contains one somewhere.
func (in JobInput) accepts(v string) bool {
	if !InputValueAllowed(v) {
		return false
	}
	if len(in.Enum) > 0 {
		for _, allowed := range in.Enum {
			if v == allowed {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile("^(?:" + in.Pattern + ")$")
	if err != nil {
		return false
	}
	return re.MatchString(v)
}

// ValidateJobInputValues checks operator-supplied overrides against the
// declaration. Defaults were checked at load; this is the other half, run on
// the workstation before anything reaches the host.
func ValidateJobInputValues(w Workload, values map[string]string) error {
	for _, name := range sortedKeys(values) {
		in, ok := w.Inputs[name]
		if !ok {
			return fmt.Errorf("input %s is not declared by this job", name)
		}
		if !in.accepts(values[name]) {
			return fmt.Errorf("input %s: %q is not an accepted value", name, values[name])
		}
	}
	return nil
}

func validateJobInputs(w Workload, path string) error {
	if len(w.Inputs) == 0 {
		return nil
	}
	if w.Schedule == nil {
		return errf("project_invalid", path+".inputs", "",
			"inputs belong to a scheduled job; declare schedule or remove inputs")
	}
	if w.DataEffect != DataEffectNone {
		return errf("project_invalid", path+".inputs", "",
			"inputs require data_effect %q; a %q job keeps the sealed plan of ob job run for operator-initiated runs",
			DataEffectNone, w.DataEffect)
	}
	for _, name := range sortedKeys(w.Inputs) {
		ip := path + ".inputs." + name
		if err := gInputName.check(ip, name); err != nil {
			return err
		}
		if strings.HasPrefix(name, ReservedInputPrefix) {
			return errf("project_invalid", ip, "", "%s is Onebox's environment namespace; choose another name", ReservedInputPrefix)
		}
		if _, clash := w.Env[name]; clash {
			return errf("project_invalid", ip, "", "input %s collides with an env key of the same name", name)
		}
		in := w.Inputs[name]
		if (len(in.Enum) > 0) == (in.Pattern != "") {
			return errf("project_invalid", ip, "", "declare exactly one of enum or pattern")
		}
		if in.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + in.Pattern + ")$"); err != nil {
				return errf("project_invalid", ip+".pattern", "", "%v", err)
			}
		}
		for i, v := range in.Enum {
			if !InputValueAllowed(v) {
				return errf("project_invalid", indexed(ip+".enum", i), "",
					"enum values may not contain quotes, backslashes or control characters and are at most %d bytes", maxInputValueBytes)
			}
		}
		if !in.accepts(in.Default) {
			return errf("project_invalid", ip+".default", "",
				"default %q does not satisfy the input's own constraint", in.Default)
		}
	}
	return nil
}
