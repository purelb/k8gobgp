#!/bin/bash
set -e

# Default values
GOBGP_SOCKET="${GOBGP_SOCKET:-/var/run/gobgp/gobgp.sock}"
GOBGP_API_HOST="${GOBGP_API_HOST:-localhost:50051}"
USE_UNIX_SOCKET="${USE_UNIX_SOCKET:-true}"

# Cleanup function for graceful shutdown
# cleanup takes the exit status to propagate. A signal-driven shutdown is a
# clean 0; a supervised process exiting on its own is not, and reporting 0 there
# left the container in "Completed" rather than "Error" no matter how gobgpd
# died. Readiness now depends on gobgpd answering gRPC, so the restart this
# triggers is what makes its death self-healing instead of a permanently
# NotReady pod.
cleanup() {
    local status="${1:-0}"
    echo "Stopping services (exit status ${status})..."
    if [ -n "$MANAGER_PID" ]; then
        kill -TERM "$MANAGER_PID" 2>/dev/null || true
    fi
    if [ -n "$GOBGPD_PID" ]; then
        kill -TERM "$GOBGPD_PID" 2>/dev/null || true
    fi
    wait
    echo "Cleanup complete"
    exit "${status}"
}

# Set up signal handlers
trap 'cleanup 0' SIGTERM SIGINT SIGQUIT

# gobgpd serves its own Prometheus metrics on its own port. They are not
# proxied through or merged with the manager's endpoint on 7473 - two
# subsystems, two scrape targets.
#
# gobgpd logs an Error at startup noting that this endpoint is unauthenticated
# and reachable from anything that can route to the node. That is accurate, and
# it applies equally to the manager's 7473 today; see docs/metrics.md for the
# trust boundary. --pprof-disable is a real improvement: pprof otherwise serves
# /debug/pprof/cmdline on localhost:6060, which under hostNetwork is the *host*
# loopback and is readable by every host-network pod on the node.
# Fall back to loopback, not the wildcard, when NODE_IP is unset. "${NODE_IP}:7475"
# with an empty NODE_IP is ":7475", which binds every interface - so running this
# image outside the DaemonSet that supplies NODE_IP would quietly publish the peer
# table rather than failing or staying local. The DaemonSet sets NODE_IP from
# status.hostIP; GOBGP_METRICS_HOST overrides both.
if [ -n "${GOBGP_METRICS_HOST}" ]; then
    :
elif [ -n "${NODE_IP}" ]; then
    GOBGP_METRICS_HOST="${NODE_IP}:7475"
else
    echo "NODE_IP is unset; binding gobgpd metrics to loopback only" >&2
    GOBGP_METRICS_HOST="127.0.0.1:7475"
fi
GOBGPD_ARGS="--metrics-host ${GOBGP_METRICS_HOST} --pprof-disable"

# Start gobgpd with appropriate gRPC configuration
echo "Starting gobgpd..."
if [ "$USE_UNIX_SOCKET" = "true" ]; then
    # Use Unix socket for local communication (more secure)
    # shellcheck disable=SC2086
    /usr/local/bin/gobgpd --api-hosts "unix://${GOBGP_SOCKET}" ${GOBGPD_ARGS} &
    GOBGPD_PID=$!
    GOBGP_ENDPOINT="unix://${GOBGP_SOCKET}"
else
    # Use TCP (needed for external access)
    # shellcheck disable=SC2086
    /usr/local/bin/gobgpd --api-hosts "${GOBGP_API_HOST}" ${GOBGPD_ARGS} &
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
#
# --metrics-poll-interval 60s, not the 15s default: gobgpd's own collector is
# cached at 15s, and this loop's GetTable runs as a mgmtOperation under the BGP
# write lock. Two unsynchronised 15s consumers of that lock is the worst
# arrangement, and RIB size is not a fast-moving signal.
echo "Starting k8gobgp manager with endpoint: ${GOBGP_ENDPOINT}"
/usr/local/bin/manager \
    --gobgp-endpoint="${GOBGP_ENDPOINT}" \
    --metrics-poll-interval="${METRICS_POLL_INTERVAL:-60s}" &
MANAGER_PID=$!

# Wait for either process to exit. Whichever one it was, its status becomes the
# container's: an unexpected exit here is a failure, not a completion.
wait -n $GOBGPD_PID $MANAGER_PID
EXIT_STATUS=$?

echo "A supervised process exited (status ${EXIT_STATUS}); shutting down"
cleanup "${EXIT_STATUS}"
