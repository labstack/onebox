package app

import (
	"reflect"
	"testing"
)

// A constraint that resolves to no node is silently dropped.
//
// JSONSchema applies each entry only `if at := indexPath(doc, c.path); at != nil`,
// so an entry naming a field that was renamed or withdrawn does nothing and says
// nothing. Three had rotted that way — two for the withdrawn `secrets` block and
// one for a `failure_domain.deployment` that no longer exists — while the
// published schema silently stopped constraining what the loader still refuses.
func TestEverySchemaConstraintResolvesToANode(t *testing.T) {
	defs := map[string]any{}
	root := schemaFor(reflect.TypeOf(Spec{}), defs)
	doc := map[string]any{}
	for k, v := range root {
		doc[k] = v
	}
	for _, c := range schemaConstraints {
		if indexPath(doc, c.path) == nil {
			t.Errorf("%v resolves to no node in the schema, so the constraint is dropped without a word", c.path)
		}
	}
}
