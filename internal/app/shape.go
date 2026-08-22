package app

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Closedness is a property of the type the document decodes into.
//
// A contract that accepts a field it does not define has stopped being a
// contract, and it stops without saying anything: `replicaz: 3` validated,
// deployed, and ran one replica. So the set of accepted fields is read from the
// model itself rather than declared a second time somewhere that can disagree
// with it. Adding a field to the model is the only way to make it acceptable,
// and removing one is the only way to make it rejected.
//
// The walk runs over the document after shorthand expansion, so it sees the
// same normalised shape the model describes.

// checkShape refuses any field the model does not define, naming the field, the
// line it was written on, and the defined name it is closest to.
func checkShape(raw map[string]any, lines map[string]int) error {
	return walkShape(reflect.TypeOf(Spec{}), raw, "", lines)
}

func walkShape(t reflect.Type, value any, path string, lines map[string]int) error {
	t = deref(t)

	switch t.Kind() {
	case reflect.Struct:
		body, ok := value.(map[string]any)
		if !ok {
			return nil // a scalar where a mapping is expected is the decoder's to report
		}
		allowed := fieldsOf(t)
		for _, key := range sortedKeys(body) {
			// Extension keys are accepted wherever a mapping is, and carry no
			// meaning; the contract promises they never affect the runtime.
			if strings.HasPrefix(key, "x-") {
				continue
			}
			field, known := allowed[key]
			if !known {
				return unknownField(path, key, allowed, lines)
			}
			if err := walkShape(field.Type, body[key], join2(path, key), lines); err != nil {
				return err
			}
		}
	case reflect.Map:
		body, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for _, key := range sortedKeys(body) {
			if err := walkShape(t.Elem(), body[key], join2(path, key), lines); err != nil {
				return err
			}
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for i, item := range items {
			if err := walkShape(t.Elem(), item, fmt.Sprintf("%s[%d]", path, i), lines); err != nil {
				return err
			}
		}
	}
	return nil
}

// fieldsOf is the model's own field set, keyed by the name the document uses.
// Fields the model hides from the document — the project's directory, the
// captured input — carry `json:"-"` and are absent here, so nobody can write
// them.
func fieldsOf(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f
	}
	return out
}

func unknownField(path, key string, allowed map[string]reflect.StructField, lines map[string]int) error {
	full := join2(path, key)
	msg := fmt.Sprintf("%q is not a field of this contract", key)
	if line, ok := lines[full]; ok {
		msg = fmt.Sprintf("%q is not a field of this contract (line %d)", key, line)
	}
	if near := nearestField(key, allowed); near != "" {
		msg += fmt.Sprintf("; did you mean %q?", near)
	}
	// An undefined field is the one failure an agent can resolve without
	// reading prose: it is a typo or a field from a version this binary does
	// not speak, and both are answered by the name in the message. Its own
	// code lets that be handled without parsing the sentence.
	return errf("unknown_field", full, "", "%s", msg)
}

// nearestField suggests the field the author probably meant. A rejected name is
// usually a letter or a plural away from a real one, and the difference between
// "not a field" and "did you mean replicas?" is one round trip against a
// contract nobody has memorised.
func nearestField(typo string, allowed map[string]reflect.StructField) string {
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)

	best, bestDist := "", 3 // farther than this is a different word, not a typo
	for _, name := range names {
		if d := editDistance(typo, name); d < bestDist {
			best, bestDist = name, d
		}
	}
	return best
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func join2(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// lineIndex maps a dotted document path to the line it was written on, so a
// rejection can point at the text rather than at the model.
//
// It is built from the document as authored, before shorthand expansion, which
// is the only form whose line numbers mean anything to the person reading the
// error.
func lineIndex(root *yaml.Node) map[string]int {
	out := map[string]int{}
	indexNode(root, "", out)
	return out
}

func indexNode(n *yaml.Node, path string, out map[string]int) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			indexNode(c, path, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			child := join2(path, key.Value)
			out[child] = key.Line
			indexNode(value, child, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			indexNode(c, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}
