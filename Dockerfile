# syntax=docker/dockerfile:1
#
# One Dockerfile for every Go service in the monorepo. Pick the program with PKG:
#   docker build --build-arg PKG=./cmd/hello -t tiny-bank/hello:dev .

ARG GO_VERSION=1.27.3

# --- build stage: full Go toolchain, thrown away after the build ---
# BUILDPLATFORM is the machine doing the build; Go cross-compiles to TARGETOS/TARGETARCH,
# so an arm64 Mac can build amd64 images quickly without emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
ARG PKG=./cmd/hello

WORKDIR /src

# Copy only the module files first, so the dependency download layer is cached
# until go.mod / go.sum change. (go.sum* because there are no dependencies yet.)
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# CGO_ENABLED=0 gives a static binary that needs no C libraries in the final image.
# -trimpath drops local file paths; -s -w strips debug symbols to shrink the binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app "$PKG"

# --- runtime stage: just the binary ---
# distroless/static has no shell, no package manager and no libc: very little to attack.
# The :nonroot tag runs as UID 65532 instead of root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
