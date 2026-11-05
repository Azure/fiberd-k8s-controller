# A fake helper image for hyperlight grants in kind, where there is no
# hypervisor. It holds fiberd's fakehelper, which speaks the helper protocol
# without one, at the version go.mod pins, and an empty guest, since
# fakehelper only checks that its guest exists. A grant that uses it passes
# the guest in the fiberd container's args:
#
#   runtime: hyperlight
#   helperImage: <this image>
#   ...
#     args: ["-hyperlight-guest=/var/run/fiberd/helper/guest"]
#
# Build it from the repository root and load it into the kind cluster:
#
#   docker build -t localhost/fiberd-fakehelper:kind -f hack/tests/kind/fakehelper.Dockerfile .
#   kind load docker-image localhost/fiberd-fakehelper:kind
#
# The grant names it by digest, which `docker exec kind-control-plane crictl
# images --digests` shows, since the GrantPolicy refuses a helper image
# without one. The GrantPolicy must also list localhost/fiberd-fakehelper in
# images, hyperlight in runtimes and the grant's namespace in
# privilegedNamespaces.
#
# The base images are pinned by digest. The runtime base is fiberd's own, as
# the helper image contract asks.
ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:b08bc99468058b6d8a0ada17fa54996b910d5e82a7e95bee0372e3bd98415258
ARG RUNTIME_BASE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN mkdir /out && go build -ldflags "-s -w" -o /out/helper github.com/Azure/fiberd/hack/hyperlight/fakehelper \
    && : > /out/guest

FROM ${RUNTIME_BASE}
COPY --from=build --chmod=0755 /out/helper /fiberd-helper/helper
COPY --from=build --chmod=0644 /out/guest /fiberd-helper/guest
