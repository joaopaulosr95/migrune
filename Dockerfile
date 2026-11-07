# syntax=docker/dockerfile:1

# The build stage runs on the build host's platform and cross-compiles, so
# multi-platform builds don't need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags="-s -w -X github.com/joaopaulosr95/migrune/internal/cmd.version=$VERSION" -o /out/migrune ./cmd/migrune

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/migrune /usr/local/bin/migrune
ENTRYPOINT ["/usr/local/bin/migrune"]
