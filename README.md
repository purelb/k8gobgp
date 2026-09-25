# k8gobgp

[![CI](https://github.com/purelb/k8gobgp/actions/workflows/ci.yaml/badge.svg)](https://github.com/purelb/k8gobgp/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/purelb/k8gobgp)](https://github.com/purelb/k8gobgp/releases)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/purelb/k8gobgp)](https://goreportcard.com/report/github.com/purelb/k8gobgp)

A Kubernetes controller for managing GoBGP configurations using Custom Resource Definitions (CRDs). This project implements comprehensive BGP configuration management through the Kubernetes API, leveraging the [gobgp-netlink](https://github.com/purelb/gobgp-netlink) fork (v1.3.1) for enhanced Linux kernel integration.

## Features

- **Full BGP Configuration via CRDs**: Manage all GoBGP settings declaratively through Kubernetes
- **Neighbor Management**: Configure BGP peers with full support for timers, authentication, and AFI/SAFI
- **Peer Groups**: Define reusable peer group templates for consistent neighbor configuration
- **BFD**: Sub-second forwarding-path failure detection, opt-in per neighbor or peer group
- **Community control**: Per-peer `sendCommunity` filtering of advertised community attributes
- **Dynamic Neighbors**: Support for dynamic BGP peering with prefix-based matching
- **VRF Support**: Configure Virtual Routing and Forwarding instances
- **Route Policies**: Define import/export policies with prefix lists, community matching, and AS path manipulation
- **Netlink Integration**: Import connected routes from Linux kernel and export BGP routes to routing tables, with dynamic enable/disable support (no pod restart required)
- **Security**:
  - Secret-based authentication (no plaintext passwords in CRDs)
  - Non-root container execution with minimal capabilities
  - Unix socket communication option for GoBGP
- **Per-Node Status Reporting**: `BGPNodeStatus` CRD provides cluster-wide BGP visibility — neighbor sessions, netlink import/export pipeline, RIB routes, VRFs, and health per node
- **Observability**:
  - Prometheus metrics for reconciliation, connections, and BGP state
  - Structured logging
  - Health and readiness probes
- **CRD Schema Validation**: OpenAPI v3 schema validation for configuration errors

## Installation

### Prerequisites

- Kubernetes 1.25+

### Quick Start

```bash
# Option 1: All-in-one install
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/install.yaml

# Option 2: Individual manifests
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/crds.yaml
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/rbac.yaml
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/daemonset.yaml
```

### Verifying Releases

Container images are signed with [cosign](https://github.com/sigstore/cosign).
Verification requires **cosign v3.0 or later**; cosign v2 reports `no signatures
found`, because signatures are stored in the Sigstore bundle format rather than
the legacy `.sig` tag.

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/purelb/k8gobgp/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/purelb/k8gobgp:<version>
```

Releases also carry an SPDX SBOM attestation, which can be verified with:

```bash
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp 'https://github.com/purelb/k8gobgp/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/purelb/k8gobgp:<version>
```

### From Source

```bash
# Clone the repository
git clone https://github.com/purelb/k8gobgp.git
cd k8gobgp

# Build and deploy
make docker-build docker-push IMG=<your-registry>/k8gobgp:latest
make deploy IMG=<your-registry>/k8gobgp:latest
```

## Usage

### Dynamic Router ID Resolution

The BGP router ID can be configured in three ways, providing flexibility for different deployment scenarios:

#### 1. Auto-Detection (Default)

When `routerID` is omitted, k8gobgp automatically detects the router ID from the node's primary internal IPv4 address:

```yaml
spec:
  global:
    asn: 64512
    # routerID omitted - auto-detected from node IPv4
```

For **IPv6-only nodes** (no IPv4 address available), a deterministic router ID is generated using FNV-1a hash of the node name, mapped to an address within the `routerIDPool`.

#### 2. Template Variables

Use template variables for dynamic resolution at runtime:

```yaml
spec:
  global:
    asn: 64512
    routerID: "${NODE_IP}"  # Resolved to node's internal IPv4
```

Supported template variables:

| Variable | Description |
|----------|-------------|
| `${NODE_IP}` | Node's primary internal IPv4 address |
| `${NODE_IPV4}` | Same as `${NODE_IP}` |
| `${NODE_EXTERNAL_IP}` | Node's external IPv4 address |
| `${node.annotations['key']}` | Value from node annotation |

Example using a node annotation:

```yaml
spec:
  global:
    asn: 64512
    routerID: "${node.annotations['bgp.purelb.io/router-id']}"
```

#### 3. Explicit IPv4 Address

Specify an explicit IPv4 address (backward compatible):

```yaml
spec:
  global:
    asn: 64512
    routerID: "192.168.1.1"  # Explicit IPv4
```

#### Router ID Pool for IPv6-Only Nodes

For clusters with IPv6-only nodes, configure a custom pool for hash-based router ID generation:

```yaml
spec:
  global:
    asn: 64512
    routerIDPool: "172.30.0.0/24"  # Custom pool (default: 10.255.0.0/16)
```

**Requirements:**
- Must be a valid IPv4 CIDR
- Minimum size: /24 (256 addresses)
- **Multi-cluster deployments:** Each cluster MUST use a unique `routerIDPool` to avoid router ID collisions

#### Checking Resolved Router ID

After the BGPConfiguration is reconciled, check the resolved router ID in the status:

```bash
kubectl -n k8gobgp-system get bgpconfig <name> -o yaml
```

```yaml
status:
  resolvedRouterID: "192.168.1.10"
  routerIDSource: "node-ipv4"        # explicit | template | node-ipv4 | hash-from-node-name
  routerIDNode: "worker-1"
  routerIDResolutionTime: "2026-01-26T10:00:00Z"
```

#### Router ID Immutability

Once resolved, the router ID is **immutable** for the lifetime of the pod. This prevents BGP session flapping if node annotations change. To change the router ID:

1. Update the BGPConfiguration spec
2. Delete and recreate the k8gobgp pods

### Basic BGP Configuration

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: bgp-config
  namespace: k8gobgp-system
spec:
  global:
    asn: 64512
    routerID: "192.168.1.1"  # Can be omitted for auto-detection
    listenPort: 179
    families:
      - "ipv4-unicast"

  neighbors:
    - config:
        neighborAddress: "192.168.1.254"
        peerAsn: 64513
        description: "Upstream router"
        authPasswordSecretRef:
          name: bgp-secrets
          key: upstream-password
      afiSafis:
        - family: "ipv4-unicast"
          enabled: true
      timers:
        config:
          holdTime: 90
          keepaliveInterval: 30
```

### With Netlink Integration

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: bgp-with-netlink
  namespace: k8gobgp-system
spec:
  global:
    asn: 64512
    routerID: "192.168.1.1"

  # Import connected routes from kernel
  netlinkImport:
    enabled: true
    interfaceList:
      - "eth*"
      - "ens*"

  # Export BGP routes to kernel
  netlinkExport:
    enabled: true
    dampeningInterval: 100
    routeProtocol: 186  # RTPROT_BGP
    rules:
      - name: "default-export"
        tableId: 0
        metric: 20
        validateNexthop: true

  neighbors:
    - config:
        neighborAddress: "192.168.1.254"
        peerAsn: 64513
      afiSafis:
        - family: "ipv4-unicast"
          enabled: true
```

### Authentication with Secrets

BGP MD5 authentication passwords should be stored in Kubernetes Secrets rather than in the BGPConfiguration directly. This prevents sensitive credentials from being exposed in the CR.

```yaml
# First, create the Secret with your BGP passwords
apiVersion: v1
kind: Secret
metadata:
  name: bgp-secrets
  namespace: k8gobgp-system
type: Opaque
stringData:
  # Use meaningful key names for different peers
  upstream-password: "your-md5-password"
  peer-router-1: "another-password"
```

Then reference it in your BGPConfiguration:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: bgp-with-auth
  namespace: k8gobgp-system
spec:
  global:
    asn: 64512
    routerID: "192.168.1.1"

  neighbors:
    - config:
        neighborAddress: "192.168.1.254"
        peerAsn: 64513
        description: "Authenticated upstream peer"
        # Reference the Secret and key containing the password
        authPasswordSecretRef:
          name: bgp-secrets
          key: upstream-password
      afiSafis:
        - family: "ipv4-unicast"
          enabled: true
      timers:
        config:
          holdTime: 90
          keepaliveInterval: 30
```

### Using Peer Groups

Peer groups allow you to define common settings that are inherited by multiple neighbors.

**A neighbor inherits any field it does not state.** Stating a field — to anything, including the
zero value — claims it, and the group no longer supplies it. That is what makes `authPassword: ""`
meaningful: it opts a member out of its group's TCP-MD5 password rather than meaning "unset".

Four rules are worth knowing before you rely on this, because each one has a shape that looks like
a bug if you have not met it:

| Rule | Consequence |
|---|---|
| `peerAsn` is always the group's, where the group states one | A member naming a different ASN has it discarded. The controller emits a `PeerAsnOverridden` warning event |
| `allowOwnAsn`, `replacePeerAsn` and `allowAspathLoopLocal` share one presence signal | Stating **any** of the three claims all three, so the other two fall back to their defaults instead of inheriting. State all three together, or none |
| Supplying a `timers` block claims **every** timer in it | A member that sets only `connectRetry` stops inheriting the group's `holdTime` and `keepaliveInterval` |
| `bfd`, `gracefulRestart`, `transport`, `applyPolicy` and `afiSafis` are whole-block | Omit the block to inherit it; supply it to own all of it |

`BGPNodeStatus` reports which blocks each neighbor inherited, in
`status.neighbors[].inheritedBlocks` — worth checking first when a peer has a value you did not
set, because `gobgp neighbor` shows the *resolved* configuration and cannot tell the two apart.

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: bgp-with-peer-groups
  namespace: k8gobgp-system
spec:
  global:
    asn: 64512
    routerID: "192.168.1.1"
    families:
      - "ipv4-unicast"

  # Define reusable peer group templates
  peerGroups:
    - config:
        peerGroupName: "upstream-peers"
        peerAsn: 64513
        # Peer groups can also use Secret-based authentication
        authPasswordSecretRef:
          name: bgp-secrets
          key: upstream-password
      afiSafis:
        - family: "ipv4-unicast"
          enabled: true
      timers:
        config:
          holdTime: 90
          keepaliveInterval: 30

  neighbors:
    # These neighbors inherit settings from the peer group
    - config:
        neighborAddress: "192.168.1.254"
        peerGroup: "upstream-peers"
        description: "Primary upstream"
    - config:
        neighborAddress: "192.168.1.253"
        peerGroup: "upstream-peers"
        description: "Secondary upstream"
```

### Send community

`sendCommunity` filters which community attributes are advertised to a peer. It is applied
**after** the export policy, so a policy that adds a community cannot walk past it.

```yaml
  neighbors:
    - config:
        neighborAddress: "192.168.1.254"
        peerAsn: 64513
        sendCommunity: "both"     # standard | extended | both | none
```

Omit it to leave gobgpd's behaviour alone. It is available on peer groups too, and a neighbor's
own setting overrides the group's.

Requires gobgp-netlink **v1.3.1 or later**. In every earlier release the field was declared but
dead — accepted by the config loader, dropped by the gRPC converters, and read by nothing — so
setting it changed nothing that was advertised.

#### `none` does not mean "no communities"

Three things survive every setting:

- **Large communities.** The OpenConfig enum has no value meaning "send large", so stripping them
  would make them unsendable rather than optional.
- **`LLGR_STALE` and `NO_LLGR`.** gobgpd stamps `LLGR_STALE` itself when re-advertising a stale
  route. Removing the marker while honouring the capability the peer negotiated would leave that
  peer treating stale routes as fresh.
- **Every family except IPv4/IPv6 unicast and labelled unicast.** In VPN, EVPN, FlowSpec, MUP and
  VPLS, communities are protocol payload rather than decoration — the Route Target that selects a
  VRF, and every FlowSpec traffic action, are extended communities. Stripping them would not
  filter a route, it would destroy it: a FlowSpec `discard` rule would arrive as a bare `accept`.
  There is no per-family form of this setting in the OpenConfig model, so it is ignored there.

It is also ignored entirely for route-server clients, which get no egress attribute
transformation at all (RFC 7947). gobgpd warns at startup when the setting is inert for a peer.

#### Two things to weigh before setting it

- **`none` and `extended` strip `NO_EXPORT`, `NO_ADVERTISE` and `NO_EXPORT_SUBCONFED`.** The
  receiving AS loses the signal not to re-export the route. This matches how the setting behaves
  on other vendors, but it is a route-leak vector — not merely a narrower advertisement.
- **Within unicast, `color` and `encap` extended communities are stripped along with the rest.**

Unlike most neighbor configuration, changing `sendCommunity` on a live peer does **not** reset
the session: gobgpd applies it in place and soft-resets outbound. Every other field in this
section still tears the session down.

`bgp_peer_send_community` on the `:7475` endpoint reports the configured value, and is absent for
peers that do not set it. It reports configuration, not effect — see
[docs/metrics.md](docs/metrics.md).

### BFD (Bidirectional Forwarding Detection)

BFD detects a dead forwarding path in under a second, where BGP's own hold timer
takes tens of seconds. It is off by default and opt-in per neighbor.

```yaml
  peerGroups:
    - config:
        peerGroupName: "upstream-peers"
        peerAsn: 64513
      bfd:
        enabled: true

  neighbors:
    # Inherits the peer group's BFD.
    - config:
        neighborAddress: "192.168.1.254"
        peerGroup: "upstream-peers"

    # Opts out of it. See "inheritance is whole-block" below.
    - config:
        neighborAddress: "192.168.1.253"
        peerGroup: "upstream-peers"
      bfd:
        enabled: false

    # Standalone, with explicit timers.
    - config:
        neighborAddress: "192.168.1.252"
        peerAsn: 64513
      bfd:
        enabled: true
        desiredMinimumTxInterval: 1000000   # microseconds
        requiredMinimumReceive: 1000000     # microseconds
        detectionMultiplier: 3
```

**Timers are microseconds.** `1000000` is one second; `300000` is 300ms; `300`
is 300 *microseconds* and is rejected. Detection time is
`requiredMinimumReceive x detectionMultiplier`, so the defaults above give 3s.

| Field | Default | Range |
|---|---|---|
| `enabled` | unset (inherit) | — |
| `port` | 3784 | 1024–65535 |
| `desiredMinimumTxInterval` | 1000000 (1s) | 300000–30000000 |
| `requiredMinimumReceive` | 1000000 (1s) | 300000–30000000 |
| `detectionMultiplier` | 3 | 3–255 |

The local listener is always on port 3784 regardless of `port`, which only sets
the destination for packets *sent* to that peer. RFC 5881 mandates 3784; there
is rarely a reason to change it.

#### Before you enable it

**A BFD transition is a hard BGP reset.** Not a graceful restart — the session
drops and every route through it is withdrawn. That is the point of BFD, but it
means an over-aggressive profile converts a transient scheduling hiccup into a
real outage.

**Changing any BFD field also resets the session.** gobgpd implements a BFD
config change as delete-then-add, which stops our transmit long enough for the
remote end's detect timer to expire, so *the peer* tears the session down too.
Apply BFD changes in a maintenance window, and roll them out one node at a time.

**Sub-second profiles are not viable under the shipped CPU limit.** The
DaemonSet sets `limits.cpu: 500m` against a `requests.cpu: 100m`. With the
default 100ms CFS period that is a 50ms quota, so a container that exhausts its
quota early in a period is frozen for up to 50ms:

- At the **default** 1s/3 profile, detection is 3000ms and a worst-case 50ms
  freeze is 1.7% of it. Comfortable.
- At the **300ms floor**, detection is 900ms and the same freeze is 5.6% of it —
  and, more to the point, it is a sixth of a single 300ms transmit interval, so
  a few unlucky periods in a row can cost the packets that trigger a false
  session-down and a hard BGP reset.

If you need a sub-second profile, set `requests.cpu == limits.cpu` so the
container is not throttled at the limit, and validate under load before relying
on it.

**Link-local peers need their scope zone.** A BFD control packet to `fe80::1`
has no interface to leave by, and the send still succeeds on an arbitrary one.
BGP establishes, everything reads healthy, and the BFD session never leaves
`Down` — the failure detector you just enabled is not running. Write
`fe80::1%eth0`, or use `neighborInterface`, which resolves the zone itself. The
CRD rejects the zone-less form when BFD is enabled, so this fails at `kubectl
apply` rather than in production.

**Inheritance is whole-block, not per-field.** A neighbor with no `bfd:` takes
its peer group's entire block; a neighbor with one replaces it entirely rather
than merging field by field. `enabled` is deliberately nullable so that
`bfd: {enabled: false}` is distinguishable from an absent block, which is what
makes the opt-out above work.

#### Observing it

```bash
# Per-neighbor session state, and whether the node's BFD socket is bound.
kubectl get bgpnodestatus <node> -o jsonpath='{.status.neighbors[*].bfd}'
kubectl get bgpnodestatus <node> -o jsonpath='{.status.bfdServer}'

# Ground truth. BGPNodeStatus is a 60s-granularity view of a subsystem that
# detects failure in under a second — use it to see what BFD is configured to do
# and whether it has been failing, not as the failure detector.
kubectl exec -n k8gobgp-system <pod> -c gobgpd -- gobgp neighbor <address>
```

A `BFDDegraded` condition appears on `BGPNodeStatus` only on nodes that actually
use BFD, with reason `ServerNotListening` (peers want BFD and the socket is not
bound — no failure detection is running) or `SessionsDown`. Nodes with no BFD
peers carry no BFD condition at all, and never bind the socket; that is normal,
and it does not affect `healthy`.

Metrics are on the gobgpd endpoint (`:7475`): `bgp_bfd_server_up`,
`bgp_peer_bfd_enabled`, `bgp_peer_bfd_failure_transitions_total`,
`bgp_bfd_unknown_peer_total`, `bgp_bfd_received_drop_total`,
`bgp_bfd_wrong_hop_limit_total`. See [docs/metrics.md](docs/metrics.md) and the
sample rules in [docs/alerting/k8gobgp-alerts.yaml](docs/alerting/k8gobgp-alerts.yaml).

Note that `bgp_bfd_unknown_peer_total` — the usual symptom of a zone-less
link-local peer — is a **global** counter with no peer label. Diagnosing which
peer it came from needs gobgpd's debug logs.

### Per-Node BGP Status (BGPNodeStatus)

Each k8gobgp instance automatically writes a `BGPNodeStatus` CRD for its node, providing cluster-wide BGP visibility without `kubectl exec`.

```bash
# View all node statuses
kubectl get bgpns

# Example output:
# NAME      NODE      ROUTERID          HEALTHY   NEIGHBORS   LASTUPDATED   AGE
# node-a    node-a    192.168.1.10      true      2           30s           5d
# node-b    node-b    192.168.1.11      false     2           15s           5d

# View detailed status for a node
kubectl get bgpns node-a -o yaml
```

The status includes:
- **Neighbor sessions**: state, uptime, prefixes sent/received, last error for non-Established peers
- **Inherited blocks**: `inheritedBlocks` per neighbor names what came from its peer group rather
  than from the neighbor itself. `gobgp neighbor` reports the resolved configuration, so a value the
  neighbor set and one it inherited look identical there
- **Netlink import pipeline**: interface exists/operState, imported addresses with RIB membership
- **BGP RIB**: local and received routes with next-hop and communities
- **Netlink export pipeline**: export rules, exported routes with kernel installation status
- **VRF summary**: route counts per VRF
- **Health**: `healthy` boolean + standard Kubernetes conditions

#### Configuration

Status reporting is enabled by default. Configure via the `nodeStatus` section in `spec.global`:

```yaml
spec:
  global:
    asn: 64512
    nodeStatus:
      enabled: true         # default: true. Set false to disable.
      heartbeatSeconds: 60  # default: 60. Periodic status write interval. Minimum: 10.
```

The `heartbeatSeconds` value is echoed in the status so consumers can calculate staleness:
- **60s** (default): good for most clusters
- **10s**: near-real-time, useful during debugging
- **300s**: minimal API server load for large clusters

#### Lifecycle

- **Startup**: creates `BGPNodeStatus` with an OwnerReference to the Node (enables GC on node removal)
- **Running**: writes status on state change; heartbeat ensures `lastUpdated` advances even when stable
- **Shutdown**: sets condition `Ready=False, reason=AgentShutdown` (does **not** delete the object, so rolling updates don't create visibility gaps)

## Configuration Reference

See the [config/samples/](config/samples/) directory for comprehensive examples including:

- Basic neighbor configuration
- Netlink import/export
- Route policies and defined sets
- Peer groups and dynamic neighbors
- Authentication with Secrets

### Important Notes

- **One BGPConfiguration per node**: gobgpd is a singleton on each node, and each
  reconcile deletes everything gobgpd holds that the configuration being
  reconciled does not ask for. Two `BGPConfiguration` resources would therefore
  delete each other's neighbors, peer groups, VRFs and policies on every pass —
  tearing down live BGP sessions. Because every agent watches every namespace,
  this means **one `BGPConfiguration` for the whole cluster**; use the
  per-neighbor and per-peer-group `nodeSelector` to vary configuration between
  nodes, not a second resource.

  If more than one exists, the oldest wins (tie-broken by namespace then name)
  and every other one is ignored: it applies no BGP state, reports
  `Ready=False` with reason `MultipleConfigurations`, and emits a
  `MultipleConfigurations` warning event naming the owner. Deleting an ignored
  configuration does not disturb the one in effect. If the owning configuration
  is deleted, the next-oldest takes over within 30 seconds.
- **BFD changes reset the session**: enabling, disabling or retuning BFD on a
  neighbor tears the BGP session down and rebuilds it. Apply in a maintenance
  window. See [BFD](#bfd-bidirectional-forwarding-detection).
- **Global Configuration Changes**: Changes to `global.asn` or `global.routerID` require a pod
  restart to take effect. These are immutable at runtime in GoBGP, and the reconcile stops with an
  error rather than continuing against a speaker that is not the one the CR describes.

  Every other `global.*` setting also needs a restart, but does **not** stop the reconcile — the rest
  of the configuration still applies. Drift is reported as a `GlobalRestartRequired` event and as
  `k8gobgp_global_restart_required{field}`, which stays up for as long as the drift does.
- **Multipath requires a limit, and a limit requires multipath**: `global.useMultiplePaths` must be
  paired with at least one of `global.ebgpMaximumPaths` or `global.ibgpMaximumPaths`, and neither
  limit is accepted without it. gobgpd refuses both halves inside `StartBgp`, which means the daemon
  does not start at all — so this is validated before it is sent, with an error naming the CRD
  fields. Multipath with no limit selects only the single best path; a limit with no multipath does
  nothing.
- **Timer changes reset the session**: editing `holdTime` or `keepaliveInterval` on a neighbor tears
  its BGP session down and rebuilds it. Editing them on a **peer group** does that to every member of
  the group at once, and nothing staggers it — enable graceful restart first. `connectRetry` and
  `idleHoldTimeAfterReset` are applied in place and do not reset.
- **Settings accepted but not implemented**: `global.defaultRouteDistance`,
  `global.routeSelectionOptions.advertiseInactiveRoutes`, `.enableAigp`, `.ignoreNextHopIgpMetric`,
  `timers.minimumAdvertisementInterval` and `routeFlapDamping` are retained in the CRD but no longer
  sent to gobgpd — gobgp-netlink removed them in v1.3.5 because nothing implemented them, so they
  never had an effect. Setting one logs an `InertSettings` warning event. They are kept rather than
  removed because the CRD is a structural schema: deleting a field makes the API server prune it
  silently, so `kubectl apply` would succeed and the value would simply vanish.
- **Neighbor/Peer Group Changes**: Neighbors, peer groups, policies, and other settings can be updated dynamically without pod restart.
- **Netlink Import/Export**: Can be enabled or disabled dynamically without pod restart. When disabled, imported routes are withdrawn from the RIB.

## Architecture

k8gobgp runs as a DaemonSet, with one instance per node. Each instance:

1. Watches for `BGPConfiguration` resources in every namespace, and applies the
   oldest one (see [Important Notes](#important-notes) — one per cluster)
2. Connects to the local GoBGP daemon via gRPC (TCP or Unix socket)
3. Reconciles the desired configuration with the running BGP state
4. Reports status back to the Kubernetes API

```
┌─────────────────────────────────────────────────────────────┐
│                      Kubernetes Node                         │
│  ┌─────────────────────┐    ┌─────────────────────────────┐ │
│  │   k8gobgp Pod       │    │       gobgpd                │ │
│  │  ┌───────────────┐  │    │  ┌─────────────────────┐    │ │
│  │  │  Controller   │──┼────┼──│   gRPC API          │    │ │
│  │  └───────────────┘  │    │  └─────────────────────┘    │ │
│  │  ┌───────────────┐  │    │  ┌─────────────────────┐    │ │
│  │  │  Metrics      │  │    │  │   BGP Sessions      │────┼─┼──► Peers
│  │  └───────────────┘  │    │  └─────────────────────┘    │ │
│  └─────────────────────┘    │  ┌─────────────────────┐    │ │
│                             │  │   Netlink           │────┼─┼──► Kernel
│                             │  └─────────────────────┘    │ │
│                             └─────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

## Command-Line Flags

The manager supports the following command-line flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:7473` | Address for the metrics endpoint |
| `--health-probe-bind-address` | `:7474` | Address for health probes |
| `--gobgp-endpoint` | (env: `GOBGP_ENDPOINT`) | GoBGP gRPC endpoint (e.g., `localhost:50051` or `unix:///var/run/gobgp/gobgp.sock`) |
| `--metrics-poll-interval` | `15s` | Interval for polling RIB size from gobgpd (minimum 15s). The DaemonSet sets `60s`: gobgpd's own collector is cached at 15s and this loop takes the same BGP lock |
| `--enable-per-neighbor-metrics` | — | **Deprecated, ignored.** gobgp-netlink emits per-peer metrics natively |
| `--max-neighbors-metrics` | — | **Deprecated, ignored.** Bound per-peer cardinality at scrape time; see [docs/metrics.md](docs/metrics.md) |

## Metrics

A k8gobgp pod runs two processes and each exposes its own metrics on its own
port. They are not merged or proxied — two subsystems, two scrape targets. See
[docs/metrics.md](docs/metrics.md) for the full reference.

| Endpoint | Port | Namespace | Emitted by |
|---|---|---|---|
| Controller | `7473` `/metrics` | `k8gobgp_*` | the k8gobgp manager |
| BGP daemon | `7475` `/metrics` | `bgp_*`, `fsm_loop_*` | gobgp-netlink (`gobgpd`) |

Both are unauthenticated HTTP, and the pod runs with `hostNetwork: true`, so
both are reachable at `<nodeIP>:<port>` from anything that can route to the node
— including the BGP fabric. NetworkPolicy does not cover host-namespace ports.

The controller's own metrics on `:7473/metrics`:

### Controller metrics (`:7473`)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `k8gobgp_reconcile_total` | Counter | `name`, `namespace`, `result` | Reconciliations. `result` is `success`, `failed`, `validation_failed`, `connection_failed` or `duplicate_ignored` — note the last is normal behaviour, not a failure |
| `k8gobgp_reconcile_duration_seconds` | Histogram | `name`, `namespace` | Reconciliation duration |
| `k8gobgp_configured_objects` | Gauge | `kind`, `name`, `namespace` | What the CR asks for on this node, after nodeSelector filtering. `kind` is `neighbor`, `peer_group`, `dynamic_neighbor`, `vrf`, `policy` or `defined_set` |
| `k8gobgp_configuration_ready` | Gauge | `name`, `namespace` | 1 when the CR reconciled cleanly |
| `k8gobgp_peer_apply_errors_total` | Counter | `key`, `op` | Failures applying a peer to gobgpd. The only signal for a peer gobgpd refused — such a peer never appears in `ListPeer`, so no `bgp_*` metric describes it |
| `k8gobgp_gobgpd_connection_status` | Gauge | `endpoint` | 1 when gobgpd answered its last RPC. Driven by the readiness check |
| `k8gobgp_gobgpd_connection_errors_total` | Counter | `endpoint` | Failed attempts to reach gobgpd |
| `k8gobgp_cleanup_retries_total` | Counter | `name`, `namespace` | Retries during finalizer cleanup |
| `k8gobgp_cleanup_duration_seconds` | Histogram | `name`, `namespace` | Cleanup duration |
| `k8gobgp_rib_routes` | Gauge | `family` | Routes in the global RIB. **No gobgp-netlink equivalent** — its collectors are all per-peer and none calls `GetTable` |
| `k8gobgp_metrics_collection_duration_seconds` | Histogram | — | Time to collect RIB stats |
| `k8gobgp_metrics_collection_errors_total` | Counter | — | Collection failures |
| `k8gobgp_router_id_resolution_total` | Counter | `result` | Router ID resolution attempts |
| `k8gobgp_router_id_resolution_duration_seconds` | Histogram | — | Resolution duration. Buckets top out at 2.048s |
| `k8gobgp_router_id_info` | Gauge | `router_id`, `source`, `node`, `asn`, `name`, `namespace` | Always 1; read the labels |
| `k8gobgp_global_restart_required` | Gauge | `field`, `name`, `namespace` | 1 when a `global.*` setting in the CR differs from the running gobgpd and needs a pod restart to apply. Every compared field gets a series, set to 0 rather than deleted when it stops drifting, so an alert can use a plain `> 0`. Prefer this to the `GlobalRestartRequired` event, which fires on the write and then ages out |
| `k8gobgp_nodestatus_write_total` | Counter | `result` | BGPNodeStatus writes |
| `k8gobgp_nodestatus_collection_duration_seconds` | Histogram | — | Time to collect node status |
| `k8gobgp_nodestatus_last_successful_write_timestamp_seconds` | Gauge | — | Alert on staleness, do not gate readiness on it |
| `k8gobgp_nodestatus_object_size_bytes` | Gauge | — | Approximate serialized size |

Also on `:7473`: controller-runtime's `controller_runtime_*`, `workqueue_*` and
`rest_client_*`, plus Go runtime and process metrics.

### BGP daemon metrics (`:7475`)

72 families covering session state, routes, BFD, kernel FIB programming and loop
timing. See [docs/metrics.md](docs/metrics.md) for the full list, the label
traps, ready-made queries and a scrape-time keep-list — per-peer cardinality is
`29 + 3F` series without BFD and `33 + 3F` with, where `F` is the number of
enabled address families, and gobgp-netlink applies no cap of its own.

### Migrating from the previous metric set

Twelve `k8gobgp_*` metrics were removed because gobgp-netlink emits the same
data natively, from the daemon that owns it. The replacements are **not** a
straight rename — `neighbor=` becomes `peer=`, `state=established` becomes
`session_state=SESSION_STATE_ESTABLISHED`, and `family=ipv4_unicast` becomes
`route_family=ipv4-unicast` with the separator flipped, which means
`k8gobgp_rib_routes` and `bgp_routes_*` cannot be joined without relabeling.

[docs/metrics.md](docs/metrics.md) carries the full mapping. The short version:

| Removed | Replacement |
|---|---|
| `k8gobgp_neighbors{state}` | `count by (instance, session_state) (bgp_peer_state)` |
| `k8gobgp_neighbor_state` | `bgp_peer_state` |
| `k8gobgp_neighbor_session_flaps_total` | `delta(bgp_peer_flop_count[15m])` — a gauge, so `delta` not `increase` |
| `k8gobgp_neighbor_session_established_timestamp_seconds` | `bgp_peer_established_timestamp_seconds`, gated on `bgp_peer_state` |
| `k8gobgp_neighbor_routes_*` | `bgp_routes_*{peer, route_family}` |
| `k8gobgp_routes_*` | `sum by (instance) (bgp_routes_*)` |
| `k8gobgp_{neighbors,peer_groups,...}_configured` | `k8gobgp_configured_objects{kind="..."}` |
| `k8gobgp_router_id_source` | `count by (source) (k8gobgp_router_id_info)` |

`--enable-per-neighbor-metrics` and `--max-neighbors-metrics` are accepted and
ignored; they warn on use and will be removed. Bound per-peer cardinality at
scrape time instead.

Sample alert rules, including the queries above:
[docs/alerting/k8gobgp-alerts.yaml](docs/alerting/k8gobgp-alerts.yaml).

## Development

### Prerequisites

- Go 1.25+
- Docker
- kubectl
- [kubebuilder](https://kubebuilder.io/)

### Building

```bash
# Run tests
make test

# Build binary
make build

# Build container image
make docker-build IMG=k8gobgp:dev

# Generate CRDs and code
make generate manifests
```

### Running Locally

```bash
# Install CRDs
make install

# Run controller locally
make run
```

## Troubleshooting

### Check BGPNodeStatus

```bash
# List per-node BGP status across the cluster
kubectl get bgpns

# View detailed status for a specific node
kubectl get bgpns <node-name> -o yaml

# Check if status is stale (compare lastUpdated with heartbeatSeconds)
kubectl get bgpns -o custom-columns=NODE:.status.nodeName,HEALTHY:.status.healthy,LAST:.status.lastUpdated,HB:.status.heartbeatSeconds
```

### Check BGPConfiguration Status

```bash
kubectl -n k8gobgp-system get configs.bgp.purelb.io -o wide
kubectl -n k8gobgp-system describe config.bgp.purelb.io <name>
# Or use the short name:
kubectl -n k8gobgp-system get bgpconfig -o wide
```

### View Controller Logs

```bash
kubectl -n k8gobgp-system logs -l app.kubernetes.io/name=k8gobgp -f
```

### Check GoBGP State

Use the `gobgp` CLI inside the pod to inspect BGP state. The `GOBGP_TARGET` environment variable is pre-configured, so no flags are needed:

```bash
# Get pod name
POD=$(kubectl -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')

# Check global config
kubectl -n k8gobgp-system exec $POD -- gobgp global

# List neighbors
kubectl -n k8gobgp-system exec $POD -- gobgp neighbor

# Show neighbor details
kubectl -n k8gobgp-system exec $POD -- gobgp neighbor <peer-ip>

# Show received routes from a neighbor
kubectl -n k8gobgp-system exec $POD -- gobgp neighbor <peer-ip> adj-in

# Show advertised routes to a neighbor
kubectl -n k8gobgp-system exec $POD -- gobgp neighbor <peer-ip> adj-out
```

### Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| BGPConfiguration stuck in not-ready | Global config change | Delete and recreate pod to apply ASN/RouterID changes |
| `MultipleConfigurations` status/event | More than one BGPConfiguration in the cluster | Only the oldest is applied. Delete the extras and move their neighbors into the surviving resource, using `nodeSelector` to scope them per node |
| Neighbor not establishing | Authentication mismatch | Verify Secret exists and key matches |
| No routes imported | Netlink config missing | Check `netlinkImport.enabled` and interface patterns |
| ConfigurationInvalid status | Invalid community format | Communities must be `AS:VALUE` (0-65535:0-65535) |
| CRD validation error | Invalid community pattern | Check community format matches `^\d{1,5}:\d{1,5}$` |
| Router ID resolution failed | Missing NODE_NAME env | Ensure DaemonSet has `NODE_NAME` from downward API |
| Router ID shows hash-based | IPv6-only node | Expected behavior; configure `routerIDPool` if needed |
| `kubectl get bgpns` empty | Status reporting disabled | Check `spec.global.nodeStatus.enabled` in BGPConfiguration (default: true) |
| BGPNodeStatus `lastUpdated` stale | Reporter failing | Check pod logs for collection errors; verify readyz at `/readyz/nodestatus-writer` |
| BGPNodeStatus shows `AgentShutdown` | Pod was gracefully stopped | Expected during rolling updates; new pod will overwrite with fresh data |

### Migration Guide: Switching to Dynamic Router ID

If you're upgrading from an explicit `routerID` to dynamic resolution:

1. **Existing explicit routerID** - No changes required. Explicit values continue to work.

2. **Switching to auto-detection**:
   ```yaml
   # Before
   spec:
     global:
       asn: 64512
       routerID: "192.168.1.1"

   # After (remove routerID for auto-detection)
   spec:
     global:
       asn: 64512
   ```
   Then restart the k8gobgp pods: `kubectl -n k8gobgp-system rollout restart daemonset/k8gobgp`

3. **Switching to templates**:
   ```yaml
   spec:
     global:
       asn: 64512
       routerID: "${NODE_IP}"
   ```
   Restart pods to apply.

4. **Verification checklist**:
   - Confirm `NODE_NAME` env var is set in DaemonSet
   - Verify RBAC includes `nodes:get` permission
   - Check `status.resolvedRouterID` after reconciliation

## Contributing

Contributions are welcome! Please read our contributing guidelines and submit pull requests to the main repository.

## License

Copyright 2025 Acnodal Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

## Acknowledgments

- [GoBGP](https://github.com/osrg/gobgp) - The BGP implementation
- [gobgp-netlink](https://github.com/purelb/gobgp-netlink) v1.3.1 - Enhanced GoBGP fork with netlink integration
- [PureLB](https://purelb.io) - Kubernetes load balancer project
