# syntax=docker/dockerfile:1.18@sha256:dabfc0969b935b2080555ace70ee69a5261af8a8f1b4df97b9e7fbcf6722eddf
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/onebox-discovery ./cmd/onebox-discovery
COPY internal/discovery ./internal/discovery
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/onebox-discovery ./cmd/onebox-discovery

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/labstack/onebox" \
      org.opencontainers.image.description="Isolated Docker discovery controller for the Onebox managed proxy" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/onebox-discovery /onebox-discovery
# The controller is the isolated control plane: it needs the host's root-owned
# Docker socket, has no network, exposes no listener, and runs a scratch image
# containing only this read-only client. Traefik remains non-root and socketless.
# trivy:ignore:AVD-DS-0002
USER 0:0
ENTRYPOINT ["/onebox-discovery"]
