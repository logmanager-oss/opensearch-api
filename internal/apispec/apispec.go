// Package apispec exposes the OpenSearch API path templates and their allowed
// HTTP methods, generated from a pinned OpenAPI spec.
package apispec

//go:generate go -C gen run .

import (
	"sort"
	"strings"
)

// Endpoint is an API path template and the HTTP methods it accepts.
type Endpoint struct {
	Template string
	Methods  []string
}

// Suggest returns hierarchical, segment-at-a-time completions for the --path
// flag over Paths. The tool's --path is leading-slash-optional, so typed and the
// returned candidates are matched and reported without a leading slash. When
// method is non-empty, only templates accepting it (case-insensitive) are
// considered. A candidate with deeper children gets a trailing slash; a terminal
// endpoint does not. A {param} segment is surfaced as a literal hint and treated
// as terminal, since real index/policy names cannot be completed offline.
func Suggest(typed, method string) []string {
	// The shell keeps only candidates that literally start with what the user
	// typed, so mirror their leading-slash style back in the output (the runtime
	// accepts either form). A missing leading underscore cannot be mirrored the
	// same way: it is part of the real API path, not optional.
	leadingSlash := strings.HasPrefix(typed, "/")
	typed = strings.TrimPrefix(typed, "/")
	method = strings.ToUpper(method)

	seen := make(map[string]struct{}, len(Paths))
	out := make([]string, 0, len(Paths))
	for _, ep := range Paths {
		if method != "" && !containsMethod(ep.Methods, method) {
			continue
		}
		tmpl := strings.TrimPrefix(ep.Template, "/")
		if !strings.HasPrefix(tmpl, typed) {
			continue
		}

		candidate := tmpl
		hasChildren := false
		if j := strings.IndexByte(tmpl[len(typed):], '/'); j >= 0 {
			candidate = tmpl[:len(typed)+j]
			hasChildren = true
		}
		if seg := candidate[strings.LastIndexByte(candidate, '/')+1:]; strings.HasPrefix(seg, "{") {
			hasChildren = false // cannot descend past a {param} segment
		}
		if hasChildren {
			candidate += "/"
		}
		if candidate == "" {
			continue // the root path "/" normalizes to an empty, useless candidate
		}
		if leadingSlash {
			candidate = "/" + candidate
		}

		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}

	sort.Strings(out)
	return out
}

// MethodsFor returns the HTTP methods for the template exactly matching typed
// (leading slash optional), or nil when there is no exact match so the caller
// can fall back to a standard verb list.
func MethodsFor(typed string) []string {
	typed = strings.TrimPrefix(typed, "/")
	for _, ep := range Paths {
		if strings.TrimPrefix(ep.Template, "/") == typed {
			return ep.Methods
		}
	}
	return nil
}

func containsMethod(methods []string, method string) bool {
	for _, m := range methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}
