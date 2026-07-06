// Package apispec exposes the OpenSearch API path templates and their allowed
// HTTP methods, generated from a pinned OpenAPI spec.
package apispec

//go:generate go -C gen run .

import (
	"encoding/json"
	"sort"
	"strings"
)

// Endpoint is an API path template and the HTTP methods it accepts.
type Endpoint struct {
	Template string
	Methods  []string
}

// BodyField is a top-level request-body property and its JSON type ("" when the
// type is composed or otherwise not a single scalar type).
type BodyField struct {
	Name string
	Type string
}

// MatchTemplate maps a concrete path (leading slash optional) to a spec template
// in Paths, matching segment-for-segment where a {param} segment is a wildcard.
// The match with the fewest params (most literal) wins; the returned template
// keeps its leading slash so it composes with a Bodies key.
func MatchTemplate(path string) (string, bool) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", false
	}
	segs := strings.Split(path, "/")
	best := ""
	bestParams := -1
	for _, ep := range Paths {
		tsegs := strings.Split(strings.TrimPrefix(ep.Template, "/"), "/")
		if len(tsegs) != len(segs) {
			continue
		}
		params, match := 0, true
		for i, ts := range tsegs {
			if strings.HasPrefix(ts, "{") && strings.HasSuffix(ts, "}") {
				params++
				continue
			}
			if ts != segs[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// Fewest params wins; ties resolve to the first (sorted) Paths entry.
		// Overlapping OpenSearch templates differ by literal segments, so at most
		// one matches any concrete path — a tie never arises in practice.
		if bestParams == -1 || params < bestParams {
			best, bestParams = ep.Template, params
		}
	}
	if bestParams == -1 {
		return "", false
	}
	return best, true
}

// BodySkeleton returns a pretty-printed, top-level JSON request-body scaffold for
// the given path and method, preserving spec field order. ok is false when the
// path matches no template or the operation has no object body with named
// top-level fields (array and free-form bodies are not scaffolded).
func BodySkeleton(path, method string) (string, bool) {
	tmpl, ok := MatchTemplate(path)
	if !ok {
		return "", false
	}
	fields, ok := Bodies[strings.ToUpper(method)+" "+tmpl]
	if !ok {
		return "", false
	}

	var sb strings.Builder
	sb.WriteString("{\n")
	for i, f := range fields {
		name, _ := json.Marshal(f.Name) // json-escape the key; never errors for a string
		sb.WriteString("  ")
		sb.Write(name)
		sb.WriteString(": ")
		sb.WriteString(placeholder(f.Type))
		if i < len(fields)-1 {
			sb.WriteByte(',')
		}
		sb.WriteByte('\n')
	}
	sb.WriteByte('}')
	return sb.String(), true
}

func placeholder(typ string) string {
	switch typ {
	case "string":
		return `""`
	case "integer", "number":
		return "0"
	case "boolean":
		return "false"
	case "object":
		return "{}"
	case "array":
		return "[]"
	default:
		return "null"
	}
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
