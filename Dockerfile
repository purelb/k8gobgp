# Both builder stages pin themselves to $BUILDPLATFORM and cross-compile via
# GOARCH=$TARGETARCH. Building them on the *target* platform instead would run
# every Go compile under QEMU emulation for linux/arm64, which is what made the
# multi-arch release build take ~28 minutes. Only the final stage is
# target-native, because "apk add" has to run in the target rootfs.

# gobgp-netlink v1.3.0. Pinned by commit, not by tag: a git tag is mutable, and
# the fork inherited upstream's whole v1.x tag history - so "v1.2" is a
# lightweight tag on GoBGP 1.2 from 2015, one typo away from "v1.2.0". The
# commit is also the only identifier go.mod can use, because the fork keeps
# upstream's module path (github.com/osrg/gobgp/v4) while living at
# purelb/gobgp-netlink, so no tag there is a resolvable module version. CI
# asserts this SHA and the go.mod pseudo-version name the same commit.
#
# Declared before the first FROM so every stage can re-declare it. ARG scope
# ends at FROM: without the re-declaration in gobgpd_builder the ldflag below
# expands to empty and bgp_build_info ships with no commit label.
ARG GOBGP_COMMIT=8b9996553aa09e029c0c057568e0c816a8dcb9d9

# Fetch the purelb/gobgp-netlink fork once. This stage is arch-independent, so
# it is shared by both target platforms rather than cloned per-arch.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS gobgp_src

ARG GOBGP_COMMIT

RUN apk add --no-cache git

WORKDIR /gobgp_app

# init + fetch <sha> rather than clone --branch: fetching one commit avoids
# downloading every ref, and the checkout is verified below. This requires the
# commit to be reachable from a ref on the remote, so it fails closed if the
# commit is ever made unreachable.
RUN git init -q . \
 && git remote add origin https://github.com/purelb/gobgp-netlink.git \
 && git fetch --depth 1 origin ${GOBGP_COMMIT} \
 && git checkout -q --detach FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "${GOBGP_COMMIT}"

RUN go mod download

# Build GoBGP daemon and CLI
FROM gobgp_src AS gobgpd_builder

ARG TARGETARCH
ARG GOBGP_COMMIT

# -X ...version.COMMIT is what populates the commit label on bgp_build_info.
# Sourced from the build arg rather than "git rev-parse" so the value is a
# declared input and the layer stays cache-correct.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
      -ldflags="-s -w -X github.com/osrg/gobgp/v4/internal/pkg/version.COMMIT=${GOBGP_COMMIT}" \
      -o gobgpd ./cmd/gobgpd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
      -ldflags="-s -w -X github.com/osrg/gobgp/v4/internal/pkg/version.COMMIT=${GOBGP_COMMIT}" \
      -o gobgp ./cmd/gobgp

# Build the k8gobgp reconciler/controller
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS reconciler_builder

ARG TARGETARCH

WORKDIR /k8gobgp_app

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -mod=vendor -ldflags="-s -w" -o manager ./cmd/manager

# Final runtime image
FROM alpine:3.24

ARG GOBGP_COMMIT
# Records which gobgp-netlink revision this image carries, for anyone inspecting
# the artifact rather than the source.
LABEL io.purelb.gobgpd.revision="${GOBGP_COMMIT}"

# Install ca-certificates for TLS, bash for entrypoint, and iproute2 for debugging
RUN apk add --no-cache ca-certificates bash iproute2

# Create non-root user
RUN adduser -D -u 65532 -g bgp bgp

WORKDIR /

# Copy binaries
COPY --from=gobgpd_builder /gobgp_app/gobgpd /usr/local/bin/gobgpd
COPY --from=gobgpd_builder /gobgp_app/gobgp /usr/local/bin/gobgp
COPY --from=reconciler_builder /k8gobgp_app/manager /usr/local/bin/manager

# Create directory for Unix socket
RUN mkdir -p /var/run/gobgp && chown bgp:bgp /var/run/gobgp

# Copy entrypoint script
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Expose GoBGP gRPC port (TCP) and BGP port
EXPOSE 50051 179

# Note: Running as root is required for:
# - Binding to privileged port 179 (BGP)
# - Managing Linux routing tables (netlink)
# - Creating raw sockets for BGP
# The DaemonSet should use securityContext to drop unnecessary capabilities
USER root

ENTRYPOINT ["/entrypoint.sh"]
CMD []
