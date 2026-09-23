# whats-cloud-mcp

A self-hosted, multi-tenant WhatsApp gateway with an MCP server built in,
written in Go.

**One binary, one container.** The gateway owns the WhatsApp sockets
([whatsmeow](https://github.com/tulir/whatsmeow)), persists every message,
serves a REST API for your client's web platform, and mounts an MCP server at
`/mcp` over Streamable HTTP so Claude Code can read and send messages directly.
Because MCP is reachable over HTTP, it lives in the same process — no network
hop, no second credential, no second container.

## Architecture

```
  client web platform                    owner's Claude Code / Codex
          │                                          │
          │ HTTPS, Bearer wc_live_…                  │ MCP over Streamable HTTP
          │ scope: messages:send                     │ Bearer wc_live_…
          ▼                                          ▼
  ┌────────────────────────────────────────────────────────────┐
  │                      cmd/gateway                           │
  │                  (the only binary)                         │
  ├──────────────────────────────┬─────────────────────────────┤
  │ internal/httpapi             │ internal/mcpserver          │
  │   /v1/… REST routes          │   /mcp  list_chats          │
  │                              │         list_messages       │
  │                              │         send_message        │
  ├──────────────────────────────┴─────────────────────────────┤
  │ internal/auth    keys, scopes, middleware                  │
  │ internal/wa      whatsmeow, one client per tenant          │
  │ internal/store   SQLite repositories + migrations          │
  └──────────────────────────┬─────────────────────────────────┘
                             │
              ┌──────────────▼───────────────────┐
              │  one SQLite file (pure-Go driver)│
              │  tenants · api_keys · sessions   │
              │  messages · whatsmeow_*          │
              └──────────────────────────────────┘
                             │
                        WhatsApp Web
```

The MCP tools call the repositories and the session manager **in-process**.
They share the same database, the same credentials and the same tenant
isolation as the REST API.

The SQLite file holds both schemas. whatsmeow owns every `whatsmeow_*` table
and upgrades them itself; our migrations live under `internal/store/migrations`
and are tracked in `wc_schema_migrations`, so the two never collide.

## The two credential classes

They do not overlap, and neither is a stronger version of the other.

| | **Admin** | **Tenant** |
|---|---|---|
| What it is | one `ADMIN_TOKEN` from the environment | a per-tenant API key, `wc_live_<tenant>_<secret>` |
| Can | create tenants, issue and revoke keys, start pairing | whatever its scopes allow, over REST **and** MCP |
| Cannot | send messages, read conversations, use `/mcp` | touch any other tenant, or any admin endpoint |
| Stored as | an env var, compared in constant time | SHA-256 hash + a display prefix |

Scopes: `messages:send`, `messages:read`, `media:read`, `media:write`,
`sessions:read`. A grant may be exact (`messages:send`), a resource wildcard
(`media:*`) or the global wildcard (`*`). A missing scope always denies.

### Why SHA-256 and not bcrypt or argon2

These keys are 32 bytes straight out of `crypto/rand`, not human-chosen
passwords. A password KDF exists to make a low-entropy secret expensive to
brute force; against a uniformly random 256-bit secret the brute force is
already impossible, so a KDF would only burn CPU on every authenticated
request. The gateway stores a SHA-256 hash and compares with
`crypto/subtle.ConstantTimeCompare`.

### API keys belong to the tenant, not the session

This is the invariant the whole design rests on. Pairing, re-pairing and
logging out move `sessions.status` and nothing else. **Re-pairing a WhatsApp
number never invalidates or rotates an API key.** It is asserted by
`TestAPIKeySurvivesSessionRePairing` (storage) and
`TestPairingDoesNotTouchAPIKeys` (HTTP).

## Pairing flow

1. Admin creates the tenant: `POST /v1/tenants` → tenant id + the first API key
   (the plaintext is returned exactly once).
2. Admin starts pairing: `POST /v1/tenants/{id}/sessions/pair`.
   - With `{"phone": "5215550001111"}` the gateway returns an 8-character
     **pair code** that the user types on their phone. This is the default and
     preferred flow.
   - With no phone the gateway falls back to a **QR** string to scan.
3. The user confirms on their phone. whatsmeow emits `Connected`, the gateway
   flips `sessions.status` to `connected` and records the JID.
4. On every subsequent boot the gateway calls `GetAllDevices()` and reconnects
   each already-paired tenant automatically.

## Endpoints

| Method | Path | Credential | Notes |
|---|---|---|---|
| `GET` | `/healthz` | public | liveness |
| `POST` | `/v1/tenants` | admin | → `{tenant_id, api_key}`, key plaintext returned once |
| `POST` | `/v1/tenants/{id}/keys` | admin | issue an additional scoped key |
| `DELETE` | `/v1/tenants/{id}/keys/{keyID}` | admin | revoke; `204` |
| `POST` | `/v1/tenants/{id}/sessions/pair` | admin | → `{pair_code}` or `{qr}` |
| `GET` | `/v1/sessions` | `sessions:read` | status of the caller's own session |
| `POST` | `/v1/messages` | `messages:send` | `{to, body}` |
| `GET` | `/v1/chats` | `messages:read` | `?limit=` |
| `GET` | `/v1/chats/{jid}/messages` | `messages:read` | `?limit=`, newest first |
| `POST` | `/mcp` | tenant key | MCP over Streamable HTTP |

Errors always share one shape:

```json
{ "error": { "code": "forbidden", "message": "missing required scope: messages:read" } }
```

## MCP

Three tools, served at `/mcp`:

| Tool | Arguments | Required scope |
|---|---|---|
| `list_chats` | `limit?` | `messages:read` |
| `list_messages` | `chat_jid`, `limit?` | `messages:read` |
| `send_message` | `to`, `body` | `messages:send` |

The endpoint authenticates the tenant, but it cannot enforce a single scope
because the three tools differ — so **each tool checks its own scope** against
the authenticated principal and returns a tool error when it is missing. No
tool accepts a tenant identifier: the tenant always comes from the API key.

### Connecting Claude Code

```sh
claude mcp add --transport http whatscloud https://gateway.example.com/mcp \
  --header "Authorization: Bearer wc_live_…"
```

(This flag syntax was verified against `claude mcp add --help`.)

Issue that key with only the scopes the assistant needs:

```sh
curl -sX POST "$GATEWAY/v1/tenants/$TENANT_ID/keys" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"claude-code","scopes":["messages:read","messages:send"]}'
```

### Security of the MCP endpoint

The MCP Streamable HTTP specification carries an explicit security warning.
All three of its requirements are implemented:

1. **Origin validation** (`MCP_ALLOWED_ORIGINS`). The spec requires servers to
   validate the `Origin` header to prevent DNS rebinding — otherwise any web
   page the operator visits could script requests at a gateway bound to
   localhost. A disallowed origin gets `403`. A request with **no** Origin
   header is allowed, because non-browser clients (Claude Code, curl, the SDK
   client) never send one; the allowlist governs browsers only.
2. **Loopback by default** (`BIND_ADDR`, default `127.0.0.1:8080`). The spec
   says to bind localhost rather than every interface. Widening the bind is an
   explicit operator decision.
3. **Authentication.** `/mcp` uses the same tenant API-key middleware as the
   REST API, and the same scopes apply. The admin token is rejected.

The server runs the SDK's transport in **stateless** mode. That is a security
property, not only a memory one: a stateful MCP session captures the context of
the request that created it, so every later tool call in that session would act
as whoever ran `initialize`. Stateless rebuilds the session from each request's
own context, so a tool always sees the tenant that authenticated *that* call
and a replayed `Mcp-Session-Id` cannot cross tenants. (Consequence: `GET` and
`DELETE` on `/mcp` return `405`, which the spec permits — the standalone SSE
stream is optional.)

The transport itself is the SDK's. Hand-rolling Streamable HTTP would mean
reimplementing SSE event ids, per-stream resumability via `Last-Event-ID`,
`Mcp-Session-Id` lifecycle, the 202-vs-SSE-vs-JSON response choice and
404-on-expired-session — and would be frozen at one protocol revision, while
the SDK negotiates versions with backward compatibility.

## Quickstart

### Requirements

**Go 1.26.0 or newer.** `go.mau.fi/whatsmeow` declares `go >= 1.26.0` in its
`go.mod`, so an older toolchain cannot even fetch it. With the default
`GOTOOLCHAIN=auto` a Go 1.22 install will download the right toolchain by
itself on the first build.

### Local

```sh
cp env.example .env          # then fill ADMIN_TOKEN in
export $(grep -v '^#' .env | xargs)

make test
make run
```

Provision a tenant and pair a number:

```sh
# 1. create the tenant
curl -sX POST localhost:8080/v1/tenants \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme","scopes":["messages:send","messages:read","sessions:read"]}'
# → {"tenant_id":"…","api_key":{"key":"wc_live_…"}}   ← save the key, it is shown once

# 2. start pairing (pair-code flow)
curl -sX POST localhost:8080/v1/tenants/$TENANT_ID/sessions/pair \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"phone":"5215550001111"}'
# → {"mode":"code","pair_code":"ABCD1234"}   ← type this on the phone

# 3. send a message as the tenant
curl -sX POST localhost:8080/v1/messages \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"to":"5215550002222@s.whatsapp.net","body":"hello"}'
```

### Docker

```sh
cp env.example .env          # ADMIN_TOKEN is required
docker compose up -d --build
```

One service, one named volume, no Redis, no Postgres. The image is a
multi-stage build ending on `gcr.io/distroless/static-debian12`: the SQLite
driver is `modernc.org/sqlite` (pure Go), so `CGO_ENABLED=0` produces a static
binary that needs no libc at all.

Inside the container `BIND_ADDR` is `0.0.0.0:8080` — a container process
binding `127.0.0.1` is unreachable from outside its own network namespace, so
published ports would never work. The container boundary and the compose
`ports` mapping control exposure; the compose file publishes to the host's
loopback only. Put a TLS-terminating reverse proxy in front before exposing it,
since every MCP request carries a bearer token.

## Development

```sh
make test        # go test ./...
make lint        # gofmt + go vet, fails on unformatted files
make build       # the gateway binary into ./bin
make docker-build
```

Tests are table-driven and use the standard `testing` package only. The MCP
tests drive the real SDK client over the real Streamable HTTP transport against
an `httptest` server, rather than calling the tool functions directly.

The whatsmeow-backed `wa.Manager` has no unit tests on purpose — every
meaningful path needs a live socket — so `httpapi` and MCP tests run against a
`fakeSessionManager` behind the `wa.SessionManager` interface.

## Notes on search

Message search is a `LIKE` scan behind `Messages.Search`. FTS5 is deliberately
not used yet: its support in the pure-Go driver is unverified and moving to a
cgo driver would break the static build. Keeping search behind a repository
method means FTS5 can slot in later without touching any caller.

## Licensing

`go.mau.fi/whatsmeow` is MPL-2.0 and is used strictly as a dependency. Nothing
in this repository forks, vendors or modifies its source.
