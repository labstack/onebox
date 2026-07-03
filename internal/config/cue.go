package config

import (
	_ "embed"
	"fmt"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	cueyaml "cuelang.org/go/encoding/yaml"
)

//go:embed schema.cue
var schemaSrc string

// ValidateCUE checks the raw yeet.yml against the embedded CUE schema and
// rewords CUE errors into plain, located messages — CUE never reaches the
// user (design §02).
func ValidateCUE(yamlBytes []byte, filename string) error {
	ctx := cuecontext.New()
	schema := ctx.CompileString(schemaSrc, cue.Filename("yeet-schema.cue"))
	if err := schema.Err(); err != nil {
		return fmt.Errorf("internal: embedded schema broken: %w", err)
	}
	def := schema.LookupPath(cue.ParsePath("#Config"))
	if err := def.Err(); err != nil {
		return fmt.Errorf("internal: #Config missing from schema: %w", err)
	}
	file, err := cueyaml.Extract(filename, yamlBytes)
	if err != nil {
		return reword(err, filename)
	}
	data := ctx.BuildFile(file)
	if err := data.Err(); err != nil {
		return reword(err, filename)
	}
	if err := def.Unify(data).Validate(cue.Concrete(true)); err != nil {
		return reword(err, filename)
	}
	return nil
}

func reword(err error, filename string) error {
	var lines []string
	seen := map[string]bool{}
	for _, e := range cueerrors.Errors(err) {
		msg := e.Error()
		// strip the schema's own positions and #Config prefix noise
		msg = strings.ReplaceAll(msg, "#Config.", "")
		pos := e.Position()
		loc := filename
		if pos.IsValid() && strings.Contains(pos.Filename(), filename) {
			loc = fmt.Sprintf("%s:%d", filename, pos.Line())
		}
		line := fmt.Sprintf("%s: %s", loc, msg)
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
		if len(lines) >= 6 {
			break
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("%s: %v", filename, err)
	}
	return fmt.Errorf("invalid config:\n  %s", strings.Join(lines, "\n  "))
}
