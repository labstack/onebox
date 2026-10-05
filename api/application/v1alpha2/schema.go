// Package applicationv1alpha2 embeds the public Onebox Application contract.
package applicationv1alpha2

import _ "embed"

// Schema is the exact JSON Schema published for onebox.run/v1alpha2.
//
//go:embed application.schema.json
var Schema []byte
