# Both builder stages pin themselves to $BUILDPLATFORM and cross-compile via
# GOARCH=$TARGETARCH. Building them on the *target* platform instead would run
# every Go compile under QEMU emulation for linux/arm64, which is what made the
# multi-arch release build take ~28 minutes. Only the final stage is
# target-native, because "apk add" has to run in the target rootfs.

# Fetch the purelb/gobgp-netlink fork once. This stage is arch-independent, so
# it is shared by both target platforms rather than cloned per-arch.
FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS gobgp_src

RUN apk add --no-cache git

WORKDIR /gobgp_app

# Clone the purelb/gobgp-netlink fork (v1.1.2 release with unnumbered BGP gRPC fix)
RUN git clone --depth 1 --branch v1.1.2 https://github.com/purelb/gobgp-netlink.git .

RUN go mod download

# Build GoBGP daemon and CLI
FROM gobgp_src AS gobgpd_builder

ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags="-s -w" -o gobgpd ./cmd/gobgpd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags="-s -w" -o gobgp ./cmd/gobgp

# Build the k8gobgp reconciler/controller
FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS reconciler_builder

ARG TARGETARCH

WORKDIR /k8gobgp_app

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -mod=vendor -ldflags="-s -w" -o manager ./cmd/manager

# Final runtime image
FROM alpine:3.24

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
