// Package applicationv1alpha1 embeds the public Onebox Application contract.
package applicationv1alpha1

import _ "embed"

// Schema is the exact JSON Schema published for onebox.run/v1alpha1.
//
//go:embed application.schema.json
var Schema []byte
