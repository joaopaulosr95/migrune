# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

# Base images are pinned by the digest of their multi-platform index. The tag
# before the digest is only for readers and Renovate, which updates both;
# Docker pulls by digest.

# The build stage runs on the build host's platform and cross-compiles, so
# multi-platform builds don't need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags="-s -w -X github.com/joaopaulosr95/migrune/internal/cmd.version=$VERSION" -o /out/migrune ./cmd/migrune

# The debug image adds a busybox shell, for CI systems that run job scripts
# inside the image, such as GitLab CI. Built with --target debug.
FROM gcr.io/distroless/static-debian13:debug-nonroot@sha256:12c2026b21e05857a4c730b4dbf812becf02b396cb7916166c8305f45f140ab0 AS debug
COPY --from=build /out/migrune /usr/local/bin/migrune
ENTRYPOINT ["/usr/local/bin/migrune"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/migrune /usr/local/bin/migrune
ENTRYPOINT ["/usr/local/bin/migrune"]
