#!/bin/bash
set -e

# Default values
GOBGP_SOCKET="${GOBGP_SOCKET:-/var/run/gobgp/gobgp.sock}"
GOBGP_API_HOST="${GOBGP_API_HOST:-localhost:50051}"
USE_UNIX_SOCKET="${USE_UNIX_SOCKET:-true}"

# Cleanup function for graceful shutdown
cleanup() {
    echo "Received shutdown signal, stopping services..."
    if [ -n "$MANAGER_PID" ]; then
        kill -TERM "$MANAGER_PID" 2>/dev/null || true
    fi
    if [ -n "$GOBGPD_PID" ]; then
        kill -TERM "$GOBGPD_PID" 2>/dev/null || true
    fi
    wait
    echo "Cleanup complete"
    exit 0
}

# Set up signal handlers
trap cleanup SIGTERM SIGINT SIGQUIT

# Start gobgpd with appropriate gRPC configuration
echo "Starting gobgpd..."
if [ "$USE_UNIX_SOCKET" = "true" ]; then
    # Use Unix socket for local communication (more secure)
    /usr/local/bin/gobgpd --api-hosts "unix://${GOBGP_SOCKET}" &
    GOBGPD_PID=$!
    GOBGP_ENDPOINT="unix://${GOBGP_SOCKET}"
else
    # Use TCP (needed for external access)
    /usr/local/bin/gobgpd --api-hosts "${GOBGP_API_HOST}" &
    GOBGPD_PID=$!
    GOBGP_ENDPOINT="${GOBGP_API_HOST}"
fi

# Wait for gobgpd to be ready
echo "Waiting for gobgpd to be ready..."
for i in $(seq 1 30); do
    if [ "$USE_UNIX_SOCKET" = "true" ]; then
        if [ -S "${GOBGP_SOCKET}" ]; then
            echo "gobgpd is ready (socket exists)"
            break
        fi
    else
        if /usr/local/bin/gobgp neighbor 2>/dev/null; then
            echo "gobgpd is ready"
            break
        fi
    fi
    if [ $i -eq 30 ]; then
        echo "Timeout waiting for gobgpd to start"
        exit 1
    fi
    sleep 1
done

# Start the k8gobgp controller/manager
echo "Starting k8gobgp manager with endpoint: ${GOBGP_ENDPOINT}"
/usr/local/bin/manager --gobgp-endpoint="${GOBGP_ENDPOINT}" &
MANAGER_PID=$!

# Wait for either process to exit
wait -n $GOBGPD_PID $MANAGER_PID

# If one exits, cleanup and exit
cleanup
