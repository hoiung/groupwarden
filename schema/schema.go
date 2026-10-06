// Package schema embeds config.schema.json, the single source of every
// config key, default and bound. Editors can use the same file
// (`# yaml-language-server: $schema=...`).
package schema

import _ "embed" // the schema file

// Config is the JSON Schema of config.yaml.
//
//go:embed config.schema.json
var Config []byte
