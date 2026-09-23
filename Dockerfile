# syntax=docker/dockerfile:1

# ---- builder -----------------------------------------------------------------
# whatsmeow currently requires Go >= 1.26.0; see README for details.
#
# --platform=$BUILDPLATFORM pins this stage to the machine doing the building,
# NOT to the image being produced. Do not "simplify" it away: without it buildx
# runs the whole arm64 builder stage under QEMU emulation, and a Go build that
# takes seconds natively takes many minutes emulated. Go cross-compiles for free,
# so we build natively and just retarget it with GOOS/GOARCH below.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Dependencies first so the layer is cached across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Supplied automatically by buildx, one value per entry in --platform.
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 is not optional: the SQLite driver is pure Go
# (modernc.org/sqlite), which is what lets the binary run on a distroless
# static image with no libc at all. It is also what makes the cross-compile
# above a plain GOARCH switch rather than a cross-toolchain problem.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

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
