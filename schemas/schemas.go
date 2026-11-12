// Package schemas embeds the JSON Schemas shared by the deployer, UI and agents.
package schemas

import _ "embed"

//go:embed stack.schema.json
var Stack []byte
