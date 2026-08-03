package app

import (
	"encoding/json"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The canonical form is what Onebox understood. Printing it is the contract for
// "what did you make of this", replacing the habit of reading the generated
// runtime to find out.
//
// Every value carries an origin, because the difference between a value someone
// wrote and one that appeared by default is exactly what a person checking a
// production configuration needs to see. A project file shows what was typed; it
// cannot show that `replicas` is 1 because nobody said otherwise, or that
// staging is running one replica because an override says so.

// originOf reports where each leaf value came from, keyed by dotted path.
func (p *Spec) originOf() map[string]Origin {
	out := map[string]Origin{}
	canonical, err := toGeneric(p)
	if err != nil {
		return out
	}
	walkOrigins("", canonical, p.rawExpanded, p.derivedPaths, out)
	return out
}

// walkOrigins marks a leaf explicit when the authored input carried it, and
// default when it did not. Shorthand is tracked separately because a value that
// moved from the top level was written by the author, just not where it landed.
func walkOrigins(prefix string, canonical, raw any, derived map[string]Origin, out map[string]Origin) {
	cm, ok := canonical.(map[string]any)
	if !ok {
		return
	}
	rm, _ := raw.(map[string]any)

	for _, key := range sortedKeys(cm) {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		var rv any
		present := false
		if rm != nil {
			rv, present = rm[key]
		}

		switch child := cm[key].(type) {
		case map[string]any:
			walkOrigins(path, child, rv, derived, out)
		default:
			switch {
			case derived[path] != "":
				out[path] = derived[path]
			case present:
				out[path] = OriginExplicit
			default:
				out[path] = OriginDefault
			}
		}
	}
}

// OriginShorthand marks a value the author wrote at the top level that
// normalisation moved into a workload.
const OriginShorthand Origin = "shorthand"

// Canonical renders the normalised project as YAML, annotating every value that
// the author did not write where it appears.
func (r *Resolved) Canonical() ([]byte, error) {
	generic, err := toGeneric(r.Spec)
	if err != nil {
		return nil, err
	}
	origins := r.Spec.originOf()
	for path, o := range r.Origins {
		// An override is more specific than anything derived from the file, and
		// it applies to the whole subtree it patched.
		for leaf := range origins {
			if leaf == path || strings.HasPrefix(leaf, path+".") {
				origins[leaf] = o
			}
		}
		origins[path] = o
	}

	node, err := annotated("", generic, origins)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	header := "# The canonical form: what Onebox understood, for environment " + r.Env + ".\n" +
		"# Values you did not write are marked with where they came from.\n"
	return []byte(header + sb.String()), nil
}

func annotated(prefix string, v any, origins map[string]Origin) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range sortedKeys(t) {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			child, err := annotated(path, t[k], origins)
			if err != nil {
				return nil, err
			}
			if o, ok := origins[path]; ok && o != OriginExplicit {
				child.LineComment = string(o)
			}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k}, child)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range t {
			c, err := annotated(prefix, item, origins)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	default:
		n := &yaml.Node{}
		if err := n.Encode(v); err != nil {
			return nil, err
		}
		return n, nil
	}
}

// Origins returns every leaf path and its origin, sorted, for a caller that
// wants the data rather than the annotated document.
func (r *Resolved) OriginTable() [][2]string {
	origins := r.Spec.originOf()
	for path, o := range r.Origins {
		origins[path] = o
	}
	paths := make([]string, 0, len(origins))
	for p := range origins {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([][2]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, [2]string{p, string(origins[p])})
	}
	return out
}

// captureRaw records the authored input after expansion, so origins can be
// computed later without threading a marker through every field of the model.
func (p *Spec) captureRaw(raw map[string]any, derived map[string]Origin) {
	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var copied map[string]any
	if json.Unmarshal(b, &copied) != nil {
		return
	}
	p.rawExpanded = copied
	p.derivedPaths = derived
}
