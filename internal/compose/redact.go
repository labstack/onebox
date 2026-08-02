package compose

import (
	"crypto/sha256"
	"encoding/hex"

	"gopkg.in/yaml.v3"
)

// RedactEnvYAML rewrites every `environment:` VALUE in rendered compose YAML to
// a content hash (`redacted:sha256:<12hex>`), leaving keys and all other
// structure intact. Environment is the conventional home of application
// secrets, and ob's contract is that their content is never displayed or
// persisted — only a hash travels. Because the placeholder is
// derived from the value, a rotated secret still shows as a changed hash, so
// the plan diff and the apply-time drift check both stay meaningful without
// ever exposing the value.
//
// The walk is recursive and matches `environment:` ANYWHERE — under services,
// but also under top-level `x-*` extension blocks (compose-go preserves those
// verbatim, and shared anchors like `&server-env` are defined there). Missing
// even one such block would leak. Non-secret-by-convention fields (image,
// command, labels, env_file paths) are untouched, so the diff stays reviewable.
func RedactEnvYAML(rendered []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return rendered, nil
	}
	redactWalk(doc.Content[0])
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// redactWalk descends the whole tree; the value of any mapping key named
// "environment" is masked, wherever it appears.
func redactWalk(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "environment" {
				redactEnvNode(val)
				continue // don't recurse into an already-masked env block
			}
			redactWalk(val)
		}
		return
	}
	for _, c := range n.Content { // sequences and document nodes
		redactWalk(c)
	}
}

// redactEnvNode masks the values of an environment node, whether compose
// rendered it as a mapping (KEY: value) or a sequence (- KEY=value).
func redactEnvNode(env *yaml.Node) {
	if env == nil {
		return
	}
	switch env.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(env.Content); i += 2 {
			maskScalar(env.Content[i])
		}
	case yaml.SequenceNode:
		for _, item := range env.Content {
			if item.Kind == yaml.ScalarNode {
				// "KEY=value" — keep KEY=, hash the value.
				if k := splitKey(item.Value); k != "" {
					item.SetString(k + "=" + hashPlaceholder(item.Value[len(k)+1:]))
				}
			}
		}
	}
}

// maskScalar replaces a scalar value node with a hash placeholder. A null/empty
// value carries no secret, so it is left as-is.
func maskScalar(n *yaml.Node) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return
	}
	if n.Tag == "!!null" || n.Value == "" {
		return
	}
	n.SetString(hashPlaceholder(n.Value))
}

func splitKey(kv string) string {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i]
		}
	}
	return ""
}

func hashPlaceholder(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "redacted:sha256:" + hex.EncodeToString(sum[:])[:12]
}
