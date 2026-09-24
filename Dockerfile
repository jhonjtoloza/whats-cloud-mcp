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

# A skeleton of the data directory, created here only so the runtime stage can
# COPY it with the right ownership. Distroless has no shell, so it cannot mkdir
# or chown anything itself.
RUN mkdir -p /out/data/media

# ---- runtime -----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/gateway /app/gateway

# /data MUST exist in the image, owned by the user the container runs as.
#
# Docker initialises an empty named volume from whatever is at the mount point
# in the image, ownership included. Without this the path does not exist, Docker
# creates it as root:root 0755, and the nonroot process cannot write: the
# gateway dies at boot with "mkdir /data/media: permission denied". It holds
# both the SQLite database and the media directory, so both need the write bit.
#
# 65532 is distroless's nonroot uid, spelled numerically so the COPY does not
# depend on a passwd lookup.
COPY --from=builder --chown=65532:65532 /out/data /data

# Declared AFTER the directory exists: changes made to a path after VOLUME is
# declared are discarded, so declaring it earlier would throw away the ownership
# this whole block exists to set.
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
