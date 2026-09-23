package httpapi

import (
	"net/http"
	"strings"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
)

// RequireAllowedOrigin rejects requests whose Origin header is not in the
// allowlist.
//
// The MCP Streamable HTTP specification requires this: without it, any web page
// the operator visits could script requests against a gateway bound to
// localhost, because the browser would attach no Origin restriction of its own.
// That is the DNS-rebinding attack the spec calls out.
//
// A request with no Origin header is allowed: non-browser clients (Claude Code,
// curl, the MCP SDK's own HTTP client) do not send one, and a browser always
// does for cross-origin requests. Authentication is what protects those
// callers; Origin checking protects specifically against a browser being used
// as a confused deputy.
func RequireAllowedOrigin(allowed []string) func(http.Handler) http.Handler {
	// Normalise once, at construction, rather than per request.
	allowSet := make(map[string]struct{}, len(allowed))
	for _, origin := range allowed {
		origin = strings.ToLower(strings.TrimSpace(origin))
		if origin != "" {
			allowSet[origin] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Scheme and host are case insensitive; compare the whole origin
			// exactly so that "https://claude.ai.evil.com" cannot match
			// "https://claude.ai" by prefix.
			if _, ok := allowSet[strings.ToLower(origin)]; !ok {
				apierr.Forbidden(w, "origin not allowed")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
