# syntax=docker/dockerfile:1

# ---- builder -----------------------------------------------------------------
# whatsmeow currently requires Go >= 1.26.0; see README for details.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Dependencies first so the layer is cached across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is not optional: the SQLite driver is pure Go
# (modernc.org/sqlite), which is what lets the binary run on a distroless
# static image with no libc at all.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# ---- runtime -----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/gateway /app/gateway

# The volume mounted here holds the SQLite file and downloaded media.
VOLUME ["/data"]

# BIND_ADDR is 0.0.0.0 INSIDE the container because a process binding
# 127.0.0.1 there is unreachable from outside its own network namespace, so
# published ports would never work. The container boundary and the compose
# `ports` mapping are what control exposure, not this value.
ENV DB_PATH=/data/whats-cloud.db \
    MEDIA_DIR=/data/media \
    BIND_ADDR=0.0.0.0:8080 \
    PORT=8080 \
    LOG_LEVEL=info

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/app/gateway"]
