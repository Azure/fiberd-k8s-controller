# The two kind-only grant images. They are never pushed, and only the kind
# values list them in the GrantPolicy.
#
#   plain    fiberd's release image with the reference zygote, for the smoke stage
#   conform  plain with a test-hook fiberd and curl, for the conformance suite
#
# Both start from images built from fiberd's own docker/fiberd/Dockerfile at the
# fiberd under test, so criu, tini and the libraries match a release.
# FIBERD_BUILD is that file's build stage, which holds fiberd's source, its Go
# toolchain and libfiberzygote.a. FIBERD_IMAGE is its fiberd target. conform.sh
# builds them first and passes nothing from the build context.
ARG FIBERD_BUILD=localhost/fiberd-build:kind
ARG FIBERD_IMAGE=localhost/fiberd:kind

FROM ${FIBERD_BUILD} AS build
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends libssl-dev \
    && rm -rf /var/lib/apt/lists/*
# The zygote flags mirror ZYGOTE_CFLAGS, ZYGOTE_LDFLAGS and the zygote target in
# fiberd's Makefile. A process restored from a checkpoint cannot use pointer
# authentication, so arm64 turns it off.
RUN set -eu; \
    cflags="-D_FORTIFY_SOURCE=3 -O2 -fstack-protector-strong -fstack-clash-protection -fPIE -Wformat=2 -Werror=format-security"; \
    case "$(uname -m)" in aarch64|arm64) cflags="$cflags -mbranch-protection=none" ;; esac; \
    gcc $cflags -pie -Wl,-z,relro,-z,now -Wall -Wextra -pthread -DFZ_TLS \
      -o /out/bin/refzygote sdk/c/template/refzygote.c /out/lib/libfiberzygote.a -lssl -lcrypto; \
    go build -tags fiberd_testhooks -ldflags "-s -w -buildid=" -o /out/bin/fiberd-testhooks ./cmd/fiberd

FROM ${FIBERD_IMAGE} AS plain
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends libssl3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build --chmod=0755 /out/bin/refzygote /usr/local/bin/refzygote

# The controller sets only args and refuses a tenant command, so this entrypoint
# stands and the controller's flags follow it. -admin-unsafe turns on the admin
# hooks and lets fiberd admit the suite's own grant UIDs. A release binary does
# not define the flag, so it cannot start with this entrypoint.
FROM plain AS conform
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build --chmod=0755 /out/bin/fiberd-testhooks /usr/local/bin/fiberd
ENTRYPOINT ["/usr/bin/tini-static", "--", "/usr/local/bin/fiberd", "-admin-unsafe"]
