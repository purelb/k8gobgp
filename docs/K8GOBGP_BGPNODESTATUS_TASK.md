# Task: Add BGPNodeStatus CRD to k8gobgp

## Background

We are building a `kubectl purelb` krew plugin for PureLB (a Kubernetes LoadBalancer orchestrator). The plugin needs to display BGP state from every node in the cluster — neighbor sessions, netlink import/export pipeline, RIB contents, and route advertisements.

Rather than having the plugin `kubectl exec` into each k8gobgp pod (fragile text parsing, slow at scale, elevated RBAC), **k8gobgp should write its own status to a Kubernetes CRD** that the plugin simply reads.

## Why a per-node status CRD (industry precedent)

This is an established pattern in the Kubernetes networking ecosystem:

- **Calico** uses `CalicoNodeStatus` — per-node **cluster-scoped** CRD reporting BGP peer sessions, routes, and agent state (projectcalico.org/v3)
- **Cilium** uses `CiliumNode` — per-node CRD for IPAM and networking state
- **Antrea** uses `AntreaAgentInfo` — per-node CRD for agent status
- **MetalLB/FRR-k8s** uses `BGPSessionState` — per-session CRD (one per peer per node), more granular but requires multiple CRD types

We chose the per-node CRD pattern (like Calico) rather than per-session (like MetalLB) because k8gobgp needs to expose more than just session state — it needs sessions + netlinkImport pipeline + RIB + netlinkExport pipeline + VRFs. A per-session approach would require 3-4 separate CRD types. One per-node CRD keeps it simple.

The alternative of pod exec was rejected because:
- **Fragile**: parsing unversioned CLI text output from `gobgp` and `ip` breaks on version bumps
- **Slow**: N sequential HTTP upgrade connections, 200-500ms each, 50 nodes = 10-25 seconds
- **Security**: requires elevated RBAC (pods/exec), triggers audit log noise
- **Platform-dependent**: the plugin must know how to parse Linux CLI output

## What is k8gobgp

k8gobgp is a BGP sidecar that runs alongside PureLB's `lbnodeagent` DaemonSet. It:
- Runs as a container in each lbnodeagent pod (one per node)
- Reads a `BGPConfiguration` CR (`bgp.purelb.io/v1`, resource name `configs`) for its config
- Runs an embedded GoBGP instance (gRPC API on unix socket `/var/run/gobgp/gobgp.sock`)
- Uses **netlinkImport** to watch interfaces (typically `kube-lb0`) and import connected routes into BGP
- Advertises imported routes to configured BGP neighbors
- Optionally uses **netlinkExport** to install received BGP routes into the Linux kernel routing table
- Supports VRFs with per-VRF netlink import/export
- Neighbors can have `nodeSelector` fields so different nodes peer with different neighbors
- Exposes Prometheus metrics on port 7473, health on port 7474

## What you need to build

### 1. BGPNodeStatus CRD

Add a new CRD to the `bgp.purelb.io/v1` API group. One instance per node, written by k8gobgp, read by the kubectl plugin.

**Key properties:**
- Name: same as the node name (e.g., `node-a`)
- **Cluster-scoped** (like Calico's `CalicoNodeStatus`) — simpler OwnerReference GC since both Node and BGPNodeStatus are cluster-scoped, simpler plugin queries (`kubectl get bgpns` without `-n`)
- Status-only resource (spec is empty)
- OwnerReference to the Node object (enables garbage collection when nodes are removed — works cleanly since both are cluster-scoped)

Here is the complete status schema:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPNodeStatus
metadata:
  name: node-a
  ownerReferences:
  - apiVersion: v1
    kind: Node
    name: node-a
    uid: <node-uid>
    blockOwnerDeletion: false
    controller: false
spec: {}
status:
  # Identity
  nodeName: node-a
  routerID: "192.168.1.10"
  routerIDSource: "node-ipv4"       # explicit | template | node-ipv4 | hash-from-node-name
  asn: 65000
  lastUpdated: "2026-04-03T10:30:00Z"
  heartbeatSeconds: 60              # echoed from config so plugin can calculate staleness

  # Neighbor sessions
  neighborCount: 1
  neighbors:
  - address: "192.168.1.1"
    peerASN: 65001
    localASN: 65000
    state: "Established"            # Idle | Connect | Active | OpenSent | OpenConfirm | Established
    sessionUpSince: "2026-04-01T06:30:00Z"   # metav1.Time (not duration string — plugin computes display)
    prefixesSent: 12
    prefixesReceived: 0
    description: "Upstream router"
    lastError: ""                   # notification code/subcode for non-Established neighbors

  # Netlink Import state
  netlinkImport:
    enabled: true
    vrf: ""
    interfaces:
    - name: "kube-lb0"
      exists: true
      operState: "up"               # up | down (OperUnknown mapped to "up" for dummy interfaces)
    importedAddresses:
    - address: "10.100.0.1/32"
      interface: "kube-lb0"
      inRIB: true
    - address: "10.100.0.9/32"
      interface: "kube-lb0"
      inRIB: false                  # import failure
    totalImported: 2
    truncated: false

  # BGP RIB summary
  rib:
    localRoutes:                    # routes originating from this node (Path.NeighborIp == "")
    - prefix: "10.100.0.1/32"
      nextHop: "0.0.0.0"
    localRouteCount: 1
    receivedRoutes:                 # routes received from peers (Path.NeighborIp != "")
    - prefix: "10.200.0.0/24"
      nextHop: "10.100.0.1"
      fromPeer: "10.100.0.1"
      communities: ["65001:100"]
    receivedRouteCount: 1
    truncated: false

  # Netlink Export state
  netlinkExport:
    enabled: true
    protocol: 186                   # RTPROT_BGP
    dampeningInterval: 100          # ms
    rules:
    - name: "default"
      metric: 20
      tableID: 0
    exportedRoutes:                 # primary source: ListNetlinkExport gRPC (authoritative)
    - prefix: "10.200.0.0/24"
      table: "main"
      metric: 20
      installed: true               # cross-checked against kernel route table via netlink
    - prefix: "10.200.2.0/24"
      table: "main"
      metric: 20
      installed: false
      reason: "nexthop unreachable"
    totalExported: 2
    truncated: false

  # VRF summary
  vrfs:
  - name: "customer-a"
    rd: "65000:100"
    importedRouteCount: 3           # from ListPath(TABLE_TYPE_VRF, name=vrfName)
    exportedRouteCount: 2           # from ListNetlinkExport(vrf=vrfName)

  # Overall health — conditions are authoritative; healthy is a convenience summary
  healthy: true
  conditions:
  - type: "Ready"
    status: "True"
    reason: "AllNeighborsEstablished"
    message: "1 neighbor established, netlinkImport active"
    lastTransitionTime: "2026-04-03T08:00:00Z"
```

**PrintColumns for `kubectl get bgpns`:**

| Column | JSONPath | Type |
|--------|----------|------|
| Node | `.status.nodeName` | string |
| RouterID | `.status.routerID` | string |
| Healthy | `.status.healthy` | boolean |
| Neighbors | `.status.neighborCount` | integer |
| LastUpdated | `.status.lastUpdated` | date |
| Age | `.metadata.creationTimestamp` | date |

### 2. Write strategy: write-on-change with opt-out

**Default behavior: ON.** k8gobgp writes BGPNodeStatus automatically. No user action needed.

**Write-on-change**: Only write to the API server when state actually changes:
- Neighbor session state change (Established -> Active, etc.)
- Neighbor added/removed
- Route added/removed from RIB
- Address added/removed from kube-lb0
- netlinkExport route installed/failed
- Interface state change (up/down)

**Custom deep-equal comparison** (not `reflect.DeepEqual`):
- `nil` and `[]T{}` treated as equivalent (GoBGP returns nil one cycle, empty slice the next)
- Slices sorted by stable key before comparison (neighbors by address, routes by prefix)
- `LastUpdated` excluded from comparison
- `Conditions[].LastTransitionTime` excluded (only compare Type, Status, Reason, Message)

**Periodic heartbeat**: Full sync every 60 seconds even if nothing changed. Updates `lastUpdated` to confirm the agent is alive. The plugin can use `lastUpdated` age to detect stale/dead agents.

**Write jitter**: Change-driven writes include random jitter (0 to heartbeatSeconds/4) to prevent API server write storms when a cluster-wide event (e.g., upstream router flap) causes all nodes to detect changes simultaneously.

**Status write method**: Use `Status().Patch(ctx, obj, client.MergeFrom(existing))` (MergePatch). This avoids ResourceVersion 409 conflicts and works with controller-runtime's fake client for testing. (Server-side apply / `client.Apply` is broken in the fake client — upstream kubernetes/kubernetes#115598.)

**Configuration via BGPConfiguration CR**: Add a `nodeStatus` section to `spec.global`:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: default
spec:
  global:
    asn: 65000
    # ... existing fields ...

    # Node status reporting (NEW)
    nodeStatus:
      enabled: true              # default: true. Set false to disable BGPNodeStatus writes entirely.
      heartbeatSeconds: 60       # default: 60. Periodic full sync interval even when state is unchanged.
                                 # Minimum: 10. The plugin uses lastUpdated age to detect stale agents.
```

When `nodeStatus.enabled` is false, no BGPNodeStatus is created and no API server writes occur. The plugin detects this and shows: "BGP status reporting disabled (set spec.global.nodeStatus.enabled=true in BGPConfiguration)."

The heartbeat interval is configurable so operators can tune for their environment:
- **60s** (default): good for most clusters, minimal API server load
- **10s**: near-real-time status, higher load — useful during active debugging
- **300s**: very low overhead for large clusters where BGP status is rarely checked

**Load analysis (write-on-change)**:
- Steady state (BGP established, routes stable): **1 write per node per heartbeatSeconds** (heartbeat only)
- 20-node cluster at default 60s: 0.33 writes/sec — negligible
- 500-node cluster at default 60s: 8.3 writes/sec — acceptable with jitter preventing storms
- During changes (neighbor flap, route added/removed): writes happen after jitter delay (0 to heartbeat/4)
- Object size: ~3KB (small cluster) to ~30KB (100 VIPs, 50 received routes)

**Array size caps** to prevent etcd size issues with large RIBs:
- `maxLocalRoutes: 500`
- `maxReceivedRoutes: 100`
- `maxExportedRoutes: 500`
- `maxImportedAddresses: 500`

When truncated, set `truncated: true` and populate `totalXxx` count fields so the plugin knows the list is incomplete and can display "showing 100 of 4523 received routes."

### 3. Data collection strategy

**GoBGP gRPC is the primary data source.** Direct netlink (vishvananda/netlink) is used only for kernel-level verification. This keeps GoBGP as the single source of truth.

**Data flow for each collection cycle:**
```
GoBGP gRPC ─── ListPeer(EnableAdvertised: false) ──→ neighbors[] + prefixes from AfiSafi state
           ├── GetNetlink ──────────────────────────→ import/export enabled, interface list
           ├── GetNetlinkImportStats ───────────────→ import counts, errors
           ├── GetNetlinkExportStats ───────────────→ export counts, errors
           ├── ListPath(TABLE_TYPE_GLOBAL, per family) → split by Path.NeighborIp / Path.IsNetlink:
           │     Path.IsNetlink=true  ──────────────→ importedAddresses[] with inRIB=true
           │     Path.NeighborIp==""  ──────────────→ rib.localRoutes[]
           │     Path.NeighborIp!="" ───────────────→ rib.receivedRoutes[]
           ├── ListNetlinkExport ───────────────────→ netlinkExport.exportedRoutes[] (authoritative)
           ├── ListNetlinkExportRules ──────────────→ netlinkExport.rules[]
           └── ListVrf ─────────────────────────────→ vrfs[]

netlink ────── LinkByName ──────────────────────────→ interfaces[].exists, operState
           ├── AddrList ────────────────────────────→ interface addresses (for display)
           └── RouteListFiltered(proto=186, per table) → kernel cross-check for exportedRoutes[].installed
```

**Critical implementation notes:**

| Topic | Detail |
|-------|--------|
| **Import identification** | Use `Path.IsNetlink == true` + `Path.NetlinkIfName` from `ListPath(TABLE_TYPE_GLOBAL)` to identify netlink-imported routes. Do NOT cross-reference `AddrList` with `ListPath` — this has a `/32` vs configured-prefix-length mismatch on non-dummy interfaces. |
| **RIB splitting** | `TABLE_TYPE_GLOBAL` contains ALL routes (local + received). Split by `Path.NeighborIp`: empty = locally originated, non-empty = received from peer. Do NOT use `TABLE_TYPE_LOCAL` (it's the same as GLOBAL in GoBGP). |
| **Advertised counts** | Do NOT set `EnableAdvertised` on `ListPath` — it's O(paths x peers). Get advertised counts from `ListPeer` AfiSafi state instead. |
| **Export verification** | Primary source is `ListNetlinkExport` gRPC (GoBGP's authoritative record of what it exported). Use direct netlink `RouteListFiltered(protocol=186)` only as a kernel cross-check for the `installed` field. Filter by table per-rule to handle VRF tables correctly. |
| **Interface operState** | Dummy interfaces (kube-lb0) report `OperUnknown`, not `OperUp`. Map both `OperUp` and `OperUnknown` to `"up"`. Also check `link.Attrs().Flags & net.FlagUp` as fallback. |
| **IPv6 parity** | Iterate both address families (follow `getConfiguredFamilies` pattern from BGPMetricsController). `AddrList` uses `FAMILY_ALL`. Filter out `fe80::/10` link-local addresses. Normalize IPv6 prefixes via `net.ParseCIDR` for comparison. |
| **VRF route distinguisher** | `Vrf.Rd` from GoBGP is a protobuf oneof (`*RouteDistinguisher` with TwoOctetASN/IPAddress/FourOctetASN variants). Need a `formatRouteDistinguisher()` helper (reverse of existing `parseRouteDistinguisher()` in `bgpconfiguration_controller.go:1583`). |

### 4. Lifecycle

- **Startup**: Get BGPNodeStatus — if NotFound, Create with OwnerReference to Node. If it already exists (pod restart), verify OwnerReference and continue. Then immediately `Status().Patch()` to populate status.
- **Running**: Collect state every tick. Write to API server only if state changed (after jitter) or heartbeat elapsed.
- **Shutdown (graceful)**: Do NOT delete the BGPNodeStatus. Instead, set condition `Ready=False, reason=AgentShutdown` and `healthy=false`. This avoids a 30-60s visibility gap during DaemonSet rolling updates where the plugin would see the node as "missing." The new pod overwrites with fresh data on startup.
- **Shutdown (OOMKill/SIGKILL/crash)**: BGPNodeStatus persists with last-known-good data. `lastUpdated` age reveals staleness. New pod overwrites on restart.
- **Node removal**: Kubernetes GC deletes BGPNodeStatus via OwnerReference (both cluster-scoped — GC handles this correctly).
- **Opt-out**: If `spec.global.nodeStatus.enabled` is false in BGPConfiguration, no BGPNodeStatus is created and no API server writes occur.

### 5. Architecture: Separate Runnable

The reporter is implemented as a `manager.Runnable` (same pattern as `BGPMetricsController` in `bgpmetrics_controller.go`), NOT in the main reconcile loop. This ensures:
- Status writes do not block BGP configuration reconciliation
- Independent timer (heartbeat) from the reconcile loop
- Exponential backoff on failures (like `BGPMetricsController`)
- Reporter runs in its own goroutine managed by controller-runtime

**Config passing from reconciler to reporter:**
- Reconciler calls `reporter.UpdateConfig(enabled, heartbeat, routerID, routerIDSource, asn)` after each successful reconcile
- Uses `atomic.Bool`, `atomic.Int32`, `atomic.Pointer[string]`, `atomic.Uint32` — no mutexes (project constraint)
- Before first reconcile completes, reporter uses safe defaults: `enabled=true`, `heartbeat=60`, `routerID=""` (omitted from status)

### 6. RBAC changes

The existing ClusterRole `k8gobgp-manager-role` already has `nodes: get;list;watch` — sufficient for OwnerReference UID lookup.

**Add a new rule** (via kubebuilder markers, auto-generated by `make manifests`):

```yaml
- apiGroups: [bgp.purelb.io]
  resources: [bgpnodestatuses, bgpnodestatuses/status]
  verbs: [create, delete, get, list, patch, update, watch]
```

**Add a reader ClusterRole** for the kubectl plugin (`config/rbac/status_reader_role.yaml`):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8gobgp-status-reader
  labels:
    rbac.authorization.k8s.io/aggregate-to-view: "true"
rules:
- apiGroups: [bgp.purelb.io]
  resources: [bgpnodestatuses]
  verbs: [get, list, watch]
```

The `aggregate-to-view: "true"` label lets any user with the `view` ClusterRole automatically read BGPNodeStatus (appropriate since it contains no secrets).

### 7. CRD manifest

Generated by `make manifests` (controller-gen v0.19.0) from kubebuilder markers on the Go types. Output: `config/crd/bases/bgp.purelb.io_bgpnodestatuses.yaml`.

Key CRD properties:
- Group: `bgp.purelb.io`
- Version: `v1`
- Kind: `BGPNodeStatus`
- Plural: `bgpnodestatuses`
- ShortNames: `bgpns`
- Scope: `Cluster`
- Subresources: `status: {}`

### 8. Update kustomization.yaml

Add the new CRD manifest and reader role to `config/default/kustomization.yaml`:

```yaml
resources:
  - namespace.yaml
  - ../crd/bases/bgp.purelb.io_configs.yaml
  - ../crd/bases/bgp.purelb.io_bgpnodestatuses.yaml    # NEW
  - ../rbac/service_account.yaml
  - ../rbac/role.yaml
  - ../rbac/role_binding.yaml
  - ../rbac/status_reader_role.yaml                      # NEW
  - ../daemonset/gobgp-daemonset.yaml
```

### 9. Prometheus metrics for the reporter

```go
k8gobgp_nodestatus_write_total{result="success|error|skipped"}     // counter
k8gobgp_nodestatus_collection_duration_seconds                      // histogram
k8gobgp_nodestatus_last_successful_write_timestamp                  // gauge (enables simple staleness alert)
k8gobgp_nodestatus_object_size_bytes                                // gauge (early warning for size growth)
```

`k8gobgp_nodestatus_last_successful_write_timestamp` enables a simple Prometheus alert:
```
time() - k8gobgp_nodestatus_last_successful_write_timestamp > 300
```

### 10. Health integration

If the reporter has not successfully written a status within `3 * heartbeatSeconds`, contribute a failing readiness check to the manager's readyz endpoint. This surfaces reporter failures to the DaemonSet's readiness probe.

## Existing k8gobgp architecture reference

### Deployment
- Runs as sidecar container `k8gobgp` in the `lbnodeagent` DaemonSet
- Image: `ghcr.io/purelb/k8gobgp:0.2.2`
- Shares `emptyDir` volume at `/var/run/gobgp` with lbnodeagent (GoBGP gRPC socket)
- Environment variables: `NODE_NAME`, `POD_NAME`, `POD_NAMESPACE` (from downward API)
- Ports: 7473 (metrics), 7474 (health)
- Uses `lbnodeagent` ServiceAccount in `purelb-system` namespace
- Container already has `CAP_NET_ADMIN` (GoBGP needs it for netlink operations)

### BGPConfiguration CRD (existing)
- API: `bgp.purelb.io/v1`, kind `BGPConfiguration`, plural `configs`, shortname `bgpconfig`
- Typically one CR named `default` in `purelb-system`
- Contains: `spec.global` (ASN, routerID, families), `spec.neighbors` (with `nodeSelector`), `spec.netlinkImport`, `spec.netlinkExport`, `spec.vrfs`, `spec.peerGroups`, `spec.policyDefinitions`
- Status already has: `establishedNeighbors`, `neighborCount`, `conditions`, `routerIDSource`, `routerIDResolutionTime`, `observedGeneration`, `lastReconcileTime`, `message`

### Why BGPNodeStatus is a separate CRD (not on BGPConfiguration)
- BGPConfiguration is cluster-wide (one CR), but BGP state is per-node
- Each k8gobgp instance needs to write its own status independently
- Multiple writers to the same CR's status would cause conflicts
- Separate CRD scales cleanly: 1 BGPNodeStatus per node, no contention

## Design constraints

- **No locks/mutexes**: PureLB project avoids locks. Use atomic operations, channels, or single-goroutine ownership for concurrent access.
- **Leveled logging**: Info for operations, Debug (V=1) for code-level troubleshooting.
- **IPv6 parity**: All features must work equally for IPv4 and IPv6.
- **The status update should not block the main reconcile loop.** If the K8s API call to update BGPNodeStatus fails, log the error and continue — don't crash or retry aggressively. The next collection cycle will try again.
- **GoBGP gRPC as primary data source**: All BGP and netlink state should primarily come from GoBGP's gRPC API. Direct netlink calls (vishvananda/netlink) are used only for kernel-level verification (interface operState, kernel route cross-check).

## Acceptance criteria

1. BGPNodeStatus CRD is defined as cluster-scoped and deployed alongside BGPConfiguration CRD
2. Each k8gobgp instance creates a BGPNodeStatus for its node on startup (when enabled)
3. Status is written on state change (with jitter); periodic heartbeat writes `lastUpdated` even when state is stable
4. On graceful shutdown, BGPNodeStatus is NOT deleted — condition `Ready=False, reason=AgentShutdown` is set instead
5. OwnerReference to Node ensures GC on node removal (both cluster-scoped)
6. RBAC allows k8gobgp to manage BGPNodeStatus resources; reader ClusterRole shipped for plugin
7. The status accurately reflects: neighbor sessions (with lastError for non-Established), netlinkImport pipeline (interface exists/operState + addresses + RIB membership via `Path.IsNetlink`), RIB routes split by `Path.NeighborIp`, netlinkExport pipeline (from `ListNetlinkExport` gRPC + kernel cross-check), VRF summary
8. `healthy` field is false when any neighbor is not Established or any import/export failure exists
9. `conditions` follow standard K8s condition patterns; conditions are authoritative, `healthy` is a convenience summary
10. `kubectl get bgpns` shows the list of per-node statuses with Node, RouterID, Healthy, Neighbors, LastUpdated, Age columns
11. `spec.global.nodeStatus.enabled` in BGPConfiguration controls opt-out (default: true)
12. `spec.global.nodeStatus.heartbeatSeconds` in BGPConfiguration controls heartbeat interval (default: 60, minimum: 10)
13. When `nodeStatus.enabled` is false, no BGPNodeStatus is created and no API server writes occur
14. BGPConfiguration CRD schema is updated with the new `nodeStatus` fields
15. Status writes use `MergePatch` (`Status().Patch(ctx, obj, client.MergeFrom(existing))`) to avoid ResourceVersion conflicts
16. Array sizes are capped with `truncated` + `totalCount` fields to prevent etcd size issues
17. `heartbeatSeconds` is echoed in the status so the plugin can calculate staleness
18. Reporter exposes Prometheus metrics: write counts, collection duration, last successful write timestamp, object size
19. Reporter contributes failing readiness check after `3 * heartbeatSeconds` without a successful write
20. Deep-equal comparison normalizes nil/empty slices and excludes timestamps to prevent unnecessary writes
21. Change-driven writes include randomized jitter to prevent API server write storms
