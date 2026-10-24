// Package api embeds the OpenAPI document so the server can serve it at
// GET /openapi.yaml and it can never drift from the deployed binary.
package api

import _ "embed"

// OpenAPISpec is the contents of openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
