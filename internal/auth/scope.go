package auth

import (
	"fmt"
	"strings"
)

// The complete set of scopes a tenant key may carry.
const (
	ScopeMessagesSend = "messages:send"
	ScopeMessagesRead = "messages:read"
	ScopeMediaRead    = "media:read"
	ScopeMediaWrite   = "media:write"
	ScopeSessionsRead = "sessions:read"
)

// Wildcard is the grant that matches every scope.
const Wildcard = "*"

// knownScopes maps every valid resource to its valid actions.
var knownScopes = map[string]map[string]bool{
	"messages": {"send": true, "read": true},
	"media":    {"read": true, "write": true},
	"sessions": {"read": true},
}

// AllScopes lists every concrete scope the gateway understands.
func AllScopes() []string {
	return []string{
		ScopeMessagesSend,
		ScopeMessagesRead,
		ScopeMediaRead,
		ScopeMediaWrite,
		ScopeSessionsRead,
	}
}

// HasScope reports whether the granted scopes satisfy the required one.
//
// A grant may be an exact scope ("messages:send"), a resource wildcard
// ("media:*") or the global wildcard ("*"). Anything else — including an empty
// required scope — denies. Absence of a matching grant always denies; there is
// no implicit permission.
func HasScope(granted []string, required string) bool {
	required = strings.TrimSpace(required)
	if required == "" {
		return false
	}
	// A required scope is always concrete; wildcards are only meaningful on the
	// granting side.
	if strings.Contains(required, Wildcard) {
		return false
	}

	requiredResource, _, ok := splitScope(required)
	if !ok {
		return false
	}

	for _, g := range granted {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if g == Wildcard || g == required {
			return true
		}
		resource, action, ok := splitScope(g)
		if !ok {
			continue
		}
		if action == Wildcard && resource == requiredResource {
			return true
		}
	}
	return false
}

// ParseScopes turns the comma-separated storage form into a de-duplicated
// slice. It returns nil when no scope is present.
func ParseScopes(raw string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// FormatScopes renders scopes into the comma-separated storage form.
func FormatScopes(scopes []string) string {
	var kept []string
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		kept = append(kept, s)
	}
	return strings.Join(kept, ",")
}

// ValidateScopes rejects scopes the gateway does not understand, so a typo
// cannot silently produce a key that grants nothing.
func ValidateScopes(scopes []string) error {
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || s == Wildcard {
			continue
		}
		resource, action, ok := splitScope(s)
		if !ok {
			return fmt.Errorf("auth: malformed scope %q", s)
		}
		actions, known := knownScopes[resource]
		if !known {
			return fmt.Errorf("auth: unknown scope resource %q", resource)
		}
		if action == Wildcard {
			continue
		}
		if !actions[action] {
			return fmt.Errorf("auth: unknown scope action %q for resource %q", action, resource)
		}
	}
	return nil
}

// splitScope splits "resource:action" and reports whether both halves are
// non-empty.
func splitScope(scope string) (resource, action string, ok bool) {
	resource, action, found := strings.Cut(scope, ":")
	if !found || resource == "" || action == "" {
		return "", "", false
	}
	return resource, action, true
}
