// Package apispec exposes the OpenSearch API path templates and their allowed
// HTTP methods, generated from a pinned OpenAPI spec.
package apispec

//go:generate go run gen/main.go

// Endpoint is an API path template and the HTTP methods it accepts.
type Endpoint struct {
	Template string
	Methods  []string
}
