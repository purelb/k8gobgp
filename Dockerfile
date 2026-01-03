# Build GoBGP daemon and CLI from purelb/gobgp-netlink fork
FROM golang:1.24-alpine AS gobgpd_builder

RUN apk add --no-cache git

WORKDIR /gobgp_app

# Clone the purelb/gobgp-netlink fork (v1.1.1 release with full netlink gRPC API)
RUN git clone --depth 1 --branch v1.1.1 https://github.com/purelb/gobgp-netlink.git .

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gobgpd ./cmd/gobgpd
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gobgp ./cmd/gobgp

# Build the k8gobgp reconciler/controller
FROM golang:1.24-alpine AS reconciler_builder

WORKDIR /k8gobgp_app

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -ldflags="-s -w" -o manager ./cmd/manager

# Final runtime image
FROM alpine:3.20

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
