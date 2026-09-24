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
  │                              │         find_contact        │
  │                              │         sync_history        │
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

`media:read` is separate from `messages:read` on purpose: reading the text of a
conversation is not the same as pulling its files out of WhatsApp and onto this
disk. `media:write` currently guards nothing — the gateway does not send media
yet — and is left unused rather than given a route invented to justify it.

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
| `POST` | `/v1/chats/{jid}/sync` | `messages:read` | `{count?}` (default 50, max 200); backfills older history |
| `GET` | `/v1/contacts` | `messages:read` | `?q=` name, business name, phone number or group name |
| `GET` | `/v1/messages/{id}/media` | `media:read` | downloads the attachment on demand and streams it; `410` when WhatsApp has expired it |
| `POST` | `/mcp` | tenant key | MCP over Streamable HTTP |

Errors always share one shape:

```json
{ "error": { "code": "forbidden", "message": "missing required scope: messages:read" } }
```

## MCP

Six tools, served at `/mcp`:

| Tool | Arguments | Required scope |
|---|---|---|
| `list_chats` | `limit?` | `messages:read` |
| `list_messages` | `chat_jid`, `limit?` | `messages:read` |
| `send_message` | `to`, `body` | `messages:send` |
| `find_contact` | `query` | `messages:read` |
| `sync_history` | `chat_jid`, `count?` | `messages:read` |
| `get_media` | `message_id` | `media:read` |

The intended reading sequence is `find_contact` → `list_messages` → and, when
the stored conversation is missing or too shallow, `sync_history` followed by
`list_messages` again. The tool descriptions say so, because a model has no
other way to learn it.

`find_contact` resolves a **group** by the name it shows on WhatsApp, and
`list_chats` reports that name alongside the JID. A group has no entry in the
address book and is addressed only as `120363424550223300@g.us`, so without a
cached name nothing could answer "the group called obd2ip". The names live in
the `chats` table, written from the group metadata whatsmeow reports on
connect, on join and on rename — see [Chat names](#chat-names).

The endpoint authenticates the tenant, but it cannot enforce a single scope
because the tools differ — so **each tool checks its own scope** against
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
docker compose up -d         # pulls ghcr.io/jhonjtoloza/whats-cloud-mcp:latest
```

To build from this working tree instead of pulling, add the development
override (see [Deployment](#deployment)):

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
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

## Deployment

The deployment box is small and shared with two other projects, so it does not
build anything. CI builds the image and publishes it; the server only pulls.

### Where the image lives, and why there

**GitHub Container Registry** — `ghcr.io/jhonjtoloza/whats-cloud-mcp`.

- GitHub Packages is free for **public** packages: no storage quota and no
  data-transfer quota, and container registry storage and bandwidth are
  currently free regardless.
- Docker Hub's free tier caps anonymous pulls at 100 per 6 hours per IP address
  and 200 authenticated. A shared server sits behind one IP and shares that
  budget with everything else running on it, which is exactly the failure mode
  you discover during a redeploy at the worst possible moment.
- ghcr.io is in the same place as the source, and `GITHUB_TOKEN` already has
  push rights — no personal access token and no repository secret to rotate.

### What CI publishes

`.github/workflows/publish.yml` runs on pushes to `main`, on `v*` tags and on
manual dispatch. It runs `go vet ./...` and `go test -race ./...` **before**
touching the registry: a green build is the precondition for an image existing
at all.

| Trigger | Tags pushed |
|---|---|
| push to `main` | `latest`, `sha-<short>` |
| push of `v1.4.2` | `1.4.2`, `1.4`, `sha-<short>` |
| `workflow_dispatch` | **none** — it builds both architectures as a dry run and publishes nothing |

The image is built for `linux/amd64` and `linux/arm64`. Only the builder stage
is multi-arch-aware and it runs on the *build* platform: Go cross-compiles, so
`GOARCH` comes from `TARGETARCH` and nothing is ever emulated. Building the
arm64 stage under QEMU instead would turn a seconds-long build into a
minutes-long one, which is why the `--platform=$BUILDPLATFORM` in the
Dockerfile is load-bearing and carries a comment saying so.

### Package visibility

A package published by a workflow from a **public** repository is public from
its first push: verified by pulling it with an empty Docker config and no
credentials at all. Nothing has to be changed by hand.

From a **private** repository the package starts private, and until it is made
public `docker compose pull` on the server fails with an authentication error.
Change it once, by hand:

> repository → **Packages** → `whats-cloud-mcp` → **Package settings** →
> **Change visibility** → Public

The package links itself to the repository automatically via the
`org.opencontainers.image.source` label the workflow stamps on it.

### Running it on the server

```sh
cp env.example .env          # ADMIN_TOKEN is required
docker compose up -d
```

There is no `--build` and no checkout of the source needed beyond
`docker-compose.yml` and `.env`. Pin a specific build by exporting `IMAGE_TAG`
(`IMAGE_TAG=1.4.2`, or `IMAGE_TAG=sha-1a2b3c4` to roll back to an exact
commit); it defaults to `latest`.

### Deploying on Dokploy

Use `docker-compose.prod.yml`, not `docker-compose.yml`. Dokploy routes traffic
with Traefik over the Docker network instead of through a published host port,
so two things differ: no `host:container` port mapping and no `container_name`.

Publishing on the host's loopback is correct for a server you administer
yourself and wrong here — it puts the gateway out of Traefik's reach entirely.

There is deliberately no `networks:` block. Dokploy wires networking itself:
with Isolated Deployments it creates a network named after the app and attaches
every service to it, and without isolation it adds `dokploy-network` to the
service the domain points at. Declaring it by hand only matters for a stack
with several services that must reach each other; this one has a single
service.

Attach the domain in the Dokploy UI (Domains → Create) rather than writing
Traefik labels by hand; hand-written labels fight with the ones Dokploy
injects:

> Service Name: `gateway` · Container Port: `8080`

The container port cannot collide with another service on the same box, because
nothing is published to the host and every container has its own network
namespace — only a `host:container` mapping can conflict, and there is none.
Set `APP_PORT` anyway if you want a different number; it drives both the
published container port and `BIND_ADDR`, so they cannot drift apart. Change the
domain's Container Port in the UI to match.

Then set `MCP_ALLOWED_ORIGINS` to that domain. It can stay empty while you are
testing from a terminal, because curl and Claude Code send no `Origin` header
and are always allowed — but a browser does send one, and an empty list
rejects every browser origin.

### Upgrading

```sh
docker compose pull && docker compose up -d
```

`latest` is a moving tag, so the compose file sets `pull_policy: always` to
stop a redeploy from silently reusing the stale local copy. The explicit
`pull` above is still the honest way to do it, because it fails loudly when the
registry is unreachable instead of starting the old image.

### Local development

The compose file pulls; the override builds:

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

`docker-compose.dev.yml` restores the `build:` block and sets
`pull_policy: build`. Everything else — ports, environment, volume, limits,
healthcheck, logging — comes from the base file and is not duplicated.

### Why publishing it publicly is safe

**The image contains one thing: the compiled binary.** Nothing else — the final
stage is `gcr.io/distroless/static-debian12`, and the only `COPY` into it is
`/app/gateway`.

The SQLite database and the downloaded media live in the named volume mounted
at `/data`, created at runtime on the server. `ADMIN_TOKEN` and every other
secret arrive as environment variables from `.env`, which is git-ignored and
never enters a build context. There are no credentials, no tenant data, no API
key hashes and no configuration baked into the image — pulling it gives you the
same thing as compiling the public source yourself.

## Development

```sh
make test        # go test ./...
make lint        # gofmt + go vet, fails on unformatted files
make build       # the gateway binary into ./bin
make docker-build  # build the image locally, host architecture only
make docker-dev    # build from source and start via the dev override
make docker-deploy # pull the published image and restart
```

Tests are table-driven and use the standard `testing` package only. The MCP
tests drive the real SDK client over the real Streamable HTTP transport against
an `httptest` server, rather than calling the tool functions directly.

Most of the whatsmeow-backed `wa.Manager` has no unit tests on purpose — those
paths need a live socket — so `httpapi` and MCP tests run against a
`fakeSessionManager` behind the `wa.SessionManager` interface.

The media fetcher is the exception, and deliberately so: the download itself
sits behind a one-method interface that the real `*whatsmeow.Client` satisfies,
which leaves the parts worth testing — path safety, the size guard, the type
allowlist, expiry handling, idempotency and the tenant scoping — covered without
a socket, a network or a GPU.

## History sync

WhatsApp pushes a chunk of past conversation to a newly linked device, and it
will send more on request. The gateway stores both.

**Pushed history** arrives as an event and is filtered by `HISTORY_SYNC_SCOPE`:

| Value | Stored in full |
|---|---|
| `dm` (default) | direct conversations |
| `dm,group` | direct conversations and groups |
| `all` | everything, channels included |

The scope is a volume control, not a filter on existence. A chat type outside
the scope still keeps its **most recent message**, because backfilling on
demand asks WhatsApp for the messages immediately *before* a message it can
identify: a conversation with nothing stored has no anchor and could never be
backfilled afterwards. Two things are skipped outright — status updates, which
expire in 24 hours and are not a conversation, and channels (`newsletter`)
unless the scope is `all`.

**On-demand backfill** is `POST /v1/chats/{jid}/sync` and the `sync_history`
tool. It loads the oldest message stored for the chat, asks the phone for what
came before it, waits for the reply (`HISTORY_SYNC_TIMEOUT`, 30s by default)
and reports how many messages were new. A chat with no stored message is
rejected with a `409` naming the missing anchor rather than a generic failure:
it is a constraint of WhatsApp's protocol, and the caller needs to know the fix
is to get a message into that chat first.

Both paths write through the same unique `(tenant_id, wa_message_id)` index, so
history and the live stream can never duplicate each other.

## Media

**Media is downloaded lazily and never eagerly.** Not when a message arrives,
not during a history sync, not by a background job. When a media message is
stored, the row records the *reference* — direct path, media key, both hashes,
length, mime type and CDN type — and nothing else. The bytes are fetched the
first time `GET /v1/messages/{id}/media` or the `get_media` tool asks for them.

The reason is arithmetic. Eagerly downloading every image, video and sticker a
WhatsApp account has ever received would fill a small shared server with data
nobody reads. A reference is a few hundred bytes, so keeping one for every
attachment costs nothing and keeps every message fetchable later.

It works because whatsmeow can download by reference alone, with no original
message object:

```go
func (cli *Client) DownloadMediaWithPath(ctx context.Context, directPath string,
    encFileHash, fileHash, mediaKey []byte, mediaType MediaType, mmsType string,
    allowNoHash bool) (data []byte, err error)
```

**The tradeoff, handled rather than hidden:** WhatsApp expires media
server-side, so a reference can outlive its bytes. A fetch that comes back 403,
404 or 410 means the file is gone for everyone, permanently. That is recorded as
`media_status = 'unavailable'`, answered with `410 Gone` over HTTP and a plain
"do not retry" over MCP, and **never attempted again**. Any other failure is
`failed` and may be retried. Either way only the `media_*` columns move: a
failed download never costs the message.

| Setting | Default | What it does |
|---|---|---|
| `MEDIA_DIR` | `./data/media` | root of the media tree; each tenant gets a subdirectory, mode `0750` |
| `MEDIA_MAX_BYTES` | `104857600` (100 MiB) | refuses bigger attachments, checked against the declared length *before* downloading and against the real size after |
| `MEDIA_FETCH_TYPES` | `ptt,audio,image,video,document` | which types may be downloaded |

`MEDIA_FETCH_TYPES` gates the bytes, not the bookkeeping: references are stored
for every type, stickers and gifs included, so widening the list later needs no
backfill. They are out by default because they are numerous and nobody asks an
assistant to read a sticker back.

**Path safety.** The file name is the SHA-256 of the message id, never the id
itself. WhatsApp message ids arrive from the network, and one used verbatim
could carry `../`, an absolute path or a NUL and walk out of the media
directory. Hashing removes the class instead of filtering it; the extension
comes from a fixed mime-type table; the tenant segment is charset-validated; and
the finished path is checked to be inside the tenant's own directory. Tenants
never share a directory, so one tenant's message id cannot reach another's file.

Neither the filesystem path nor any key material is ever returned over HTTP. The
`media_key` and the two hashes are secrets — they decrypt the file on WhatsApp's
CDN — so anything that dumps or exports the `messages` table is handling key
material, not metadata.

## Chat names

A person is findable by name because the address book carries one. A **group**
is not: WhatsApp keeps the subject in the group metadata, whatsmeow fetches it
live and persists none of it, so a group was only ever reachable as
`120363424550223300@g.us` — a number nobody reads on their phone.

The `chats` table is that missing index. One row per `(tenant_id, chat_jid)`
holding the display name, and it feeds two readers:

- `find_contact` / `GET /v1/contacts` match the query against it, so a group
  answers to its name. A real address-book name is never overwritten by it —
  for a person the address book is the better source.
- `list_chats` / `GET /v1/chats` report the name next to the JID. A chat with
  no name known reports none, so the caller falls back to the JID rather than
  being handed an invented one.

The name is a **cache of what WhatsApp owns**, never a source of truth, and it
is written from three events:

| When | Event | Why |
|---|---|---|
| Every successful connection | `events.Connected` → `GetJoinedGroups` | a group may have been renamed while the gateway was down, and nothing replays that |
| Joining a group | `events.JoinedGroup` | the name is already in the event |
| A rename | `events.GroupInfo` with `Name != nil` | the same event also carries topic and lock changes, which say nothing about the name |

The reconnect refresh runs off whatsmeow's event goroutine: it is a network
round trip, and taken inline it would stall every message queued behind it. A
failed refresh costs readability only — the names already cached stay, and the
next reconnect tries again.

Names are tenant-scoped like every other row. Two tenants can both belong to
one group, and each reads only the name it wrote; that is asserted by
`TestChatNameSearchIsTenantIsolated`, `TestListChatsIgnoresTheNameOfAnotherTenant`
and `TestMCPChatNamesAreTenantIsolated`.

## Notes on search

Message search is a `LIKE` scan behind `Messages.Search`. FTS5 is deliberately
not used yet: its support in the pure-Go driver is unverified and moving to a
cgo driver would break the static build. Keeping search behind a repository
method means FTS5 can slot in later without touching any caller.

## Licensing

`go.mau.fi/whatsmeow` is MPL-2.0 and is used strictly as a dependency. Nothing
in this repository forks, vendors or modifies its source.
