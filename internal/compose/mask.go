package compose

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MaskValuesYAML returns a non-executable, structure-preserving view of YAML.
// Mapping keys remain visible, while every non-empty scalar value becomes a
// keyed opaque marker. Using one random key for the live and planned documents
// makes their diff meaningful without exposing values or creating markers that
// can be brute-forced or correlated across proposals.
func MaskValuesYAML(rendered, key []byte) ([]byte, error) {
	if len(key) < 16 {
		return nil, fmt.Errorf("mask key must contain at least 16 bytes")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return rendered, nil
	}
	maskYAMLValues(doc.Content[0], key)
	return yaml.Marshal(&doc)
}

func maskYAMLValues(node *yaml.Node, key []byte) {
	if node == nil {
		return
	}
	clearYAMLComments(node)
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			// Keys describe the Compose structure; interpolation applies to values.
			clearYAMLComments(node.Content[i])
			maskYAMLValues(node.Content[i+1], key)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, child := range node.Content {
			maskYAMLValues(child, key)
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" || node.Value == "" {
			return
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(node.Tag))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(node.Value))
		sum := mac.Sum(nil)
		node.SetString("opaque:hmac-sha256:" + hex.EncodeToString(sum[:16]))
	}
}

func clearYAMLComments(node *yaml.Node) {
	if node == nil {
		return
	}
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""
}
