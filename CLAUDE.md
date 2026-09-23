# whats-cloud-mcp — project conventions

This is a **Go** project. There is no Bun, no Node, no TypeScript here.

## Toolchain

- **Go 1.26.0 or newer is required.** `go.mau.fi/whatsmeow` declares
  `go >= 1.26.0`, so an older toolchain cannot fetch it. With the default
  `GOTOOLCHAIN=auto`, an older local Go downloads the right toolchain itself.
- `go test ./...` runs the suite. `go build ./...` builds everything.
- `make test`, `make lint`, `make build` wrap the usual commands.
- `CGO_ENABLED=0` everywhere. The build must stay cgo-free.

## Testing — strict TDD

- Write the failing test **first**, watch it fail, then implement.
- Table-driven tests, standard `testing` package. No testify.
- Tests live beside the code. Handler and repository tests use `httptest` and a
  temporary SQLite file respectively — no mocking framework.
- `internal/wa`'s real whatsmeow manager is not unit tested (it needs a live
  socket). Everything above it depends on the `wa.SessionManager` interface and
  is tested against a fake.

## Architecture

```
cmd/gateway      the ONLY binary: whatsmeow sockets, REST API and MCP server
internal/config          env-based configuration
internal/store           SQLite: embedded migrations + repositories
internal/auth            key generation, hashing, scopes, HTTP middleware
internal/wa              whatsmeow behind an interface (session manager)
internal/httpapi         routes + handlers, and the /mcp mount
internal/mcpserver       MCP tools, calling the repositories in-process
internal/apierr          the single JSON error shape
```

Dependencies point inwards. `httpapi` depends on `store`, `auth`, `mcpserver`
and the `wa` interface; `wa`, `store` and `mcpserver` never import `httpapi`.

There is no separate MCP binary and no HTTP client of our own API. The MCP
server is mounted inside the gateway at `/mcp` over the SDK's Streamable HTTP
transport, and its tools call the repositories directly.

## Hard rules

- **Everything is written in English**: code, identifiers, comments, docs,
  error strings, log messages, commit messages.
- **stdlib `net/http` only.** Go 1.22 `ServeMux` pattern routing
  (`POST /v1/tenants/{id}/keys`). No chi, no gin, no echo.
- **SQLite through `modernc.org/sqlite`** (pure Go, driver name `"sqlite"`).
  Never swap in a cgo driver: the static distroless image depends on it.
- **whatsmeow is MPL-2.0.** Use it as a dependency only. Never fork, vendor or
  modify its files. whatsmeow owns every `whatsmeow_*` table in the shared
  SQLite file; our migrations are namespaced under `wc_schema_migrations`.
- **Structured logging with `log/slog`.** Never log API key plaintext or
  message bodies at info level.
- **Never hand-implement an MCP transport.** Use
  `github.com/modelcontextprotocol/go-sdk`. Register tools with the generic
  `mcp.AddTool[In, Out]` so schemas come from Go structs with `json` and
  `jsonschema` struct tags — do not hand-write JSON Schema.
- **The MCP server runs stateless.** This is a security property: a stateful
  session would pin the principal of whoever ran `initialize`, so every later
  tool call in that session would act as them. Do not switch it to stateful
  without solving per-request tenant binding first.
- **No FTS5 for now.** Content search stays behind `Messages.Search` as a LIKE
  scan so FTS5 can replace it later without touching callers.

## Security invariants

These are load-bearing. Changing them requires changing the tests that assert
them, which should be a deliberate act.

1. **API keys belong to the TENANT, not to the session.** Pairing and
   re-pairing only move `sessions.status`. A key is never invalidated or
   rotated by a re-pair.
   Guarded by `TestAPIKeySurvivesSessionRePairing` and
   `TestPairingDoesNotTouchAPIKeys`.
2. **A tenant can only ever reach its own data.** The tenant id comes from the
   stored key record, never from the request path, query or headers.
   Guarded by `TestRequireScopeBindsPrincipalToItsOwnTenant`,
   `TestMessageListByChatIsTenantIsolated` and
   `TestTenantCannotReadAnotherTenantsMessages`.
3. **Admin and tenant are separate credential classes.** The admin token cannot
   send or read messages; a tenant key, even with `*`, cannot reach an admin
   endpoint.
4. **Only the SHA-256 hash of a key is stored**, compared with
   `crypto/subtle.ConstantTimeCompare`. No bcrypt/argon2 — these are
   high-entropy random keys, not passwords, so a KDF would only waste CPU per
   request. The rationale is commented in `internal/auth/key.go`.
5. **Every MCP tool derives its tenant from the authenticated principal**, read
   from the request context. No tool takes a tenant argument; the client must
   never be able to choose whose data it reads.
   Guarded by `TestMCPTenantIsolation`,
   `TestMCPToolsIgnoreClientSuppliedTenant` and
   `TestMCPNewToolsIgnoreClientSuppliedTenant`.
6. **The /mcp endpoint enforces scopes per tool.** One URL serves tools with
   different requirements (`messages:read` for reads, `messages:send` for
   sends), so endpoint middleware only authenticates and each tool checks for
   itself. Guarded by `TestMCPEnforcesScopePerTool`.
7. **The Origin header is validated on /mcp** (`MCP_ALLOWED_ORIGINS`), and the
   server binds loopback by default (`BIND_ADDR`). Both are MCP spec
   requirements against DNS rebinding. Guarded by
   `TestRequireAllowedOrigin` and `TestMCPEndpointValidatesOrigin`.

## Before finishing any change

```sh
gofmt -l .      # must print nothing
go vet ./...
go test ./...
```
