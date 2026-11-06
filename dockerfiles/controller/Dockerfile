# The controller image, the only image this repository ships. Build it from
# the repository root.
#
#   make image             the host architecture, loaded into Docker
#   make image-multiarch   amd64 and arm64, no push
#
# The build stage runs on the build host and cross-compiles, so neither
# architecture needs emulation. CGO is off, so the binary is the same as a
# native build's.
#
# The base images are pinned by the digest of their multi-architecture index.
# Refresh them with `docker buildx imagetools inspect <image:tag>`.
ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api api
COPY cmd cmd
COPY pkg pkg
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w -buildid=" \
    -o /out/fiberd-k8s-controller ./cmd/fiberd-k8s-controller

FROM ${RUNTIME_BASE}
LABEL org.opencontainers.image.source="https://github.com/Azure/fiberd-k8s-controller" \
      org.opencontainers.image.description="The fiberd Kubernetes controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build --chmod=0755 /out/fiberd-k8s-controller /fiberd-k8s-controller
USER 65532:65532
ENTRYPOINT ["/fiberd-k8s-controller"]
