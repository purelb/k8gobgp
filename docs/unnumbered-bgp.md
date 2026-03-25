# IPv6 Unnumbered BGP with k8gobgp

IPv6 unnumbered BGP establishes BGP sessions using IPv6 link-local addresses instead of explicitly configured IP addresses. This eliminates the need to coordinate neighbor addresses between routers and Kubernetes nodes, and allows a single BGPConfiguration to work identically across nodes on different subnets.

## How It Works

In traditional (addressed) BGP, each node needs a neighbor entry with the router's specific IP address. In a multi-subnet cluster, this means different neighbors per subnet, managed with `nodeSelector`:

```
Node on 172.30.250.0/24  →  neighborAddress: 172.30.250.1
Node on 172.30.251.0/24  →  neighborAddress: 172.30.251.1
```

With unnumbered BGP, nodes peer using the IPv6 link-local address discovered on a named interface. Every node uses the same configuration regardless of subnet:

```
All nodes  →  neighborInterface: eth1
```

The router identifies each node by its link-local address on the shared L2 segment. GoBGP automatically resolves the peer's `fe80::` address from the specified interface.

### RFC 5549: Extended Next-Hop

When `neighborInterface` is set alongside the `ipv4-unicast` address family, GoBGP automatically negotiates the RFC 5549 Extended Next-Hop capability. This allows IPv4 routes to be advertised with IPv6 link-local next-hops, so IPv4 routing works over an IPv6-only peering session without any additional configuration.

## k8gobgp Configuration

Use `neighborInterface` instead of `neighborAddress` in the neighbor config:

```yaml
apiVersion: bgp.purelb.io/v1
kind: BGPConfiguration
metadata:
  name: unnumbered-bgp
  namespace: k8gobgp-system
spec:
  global:
    asn: 64515
    listenPort: 179
    families:
      - "ipv4-unicast"
      - "ipv6-unicast"

  netlinkImport:
    enabled: true
    interfaceList:
      - "kube-lb0"

  neighbors:
    - config:
        neighborInterface: "eth1"
        peerAsn: 64514
        description: "FRR router via unnumbered BGP"
      afiSafis:
        - family: "ipv4-unicast"
          enabled: true
        - family: "ipv6-unicast"
          enabled: true
      timers:
        config:
          holdTime: 90
          keepaliveInterval: 30
```

Key points:
- **`neighborInterface`** replaces `neighborAddress` — set it to the host interface facing the router (e.g., `eth1`)
- **No `nodeSelector` required** — the same interface name works on every node; the router handles subnet-level distinction on its side
- **Both address families** should be enabled; `ipv4-unicast` triggers RFC 5549 Extended Next-Hop negotiation
- **`netlinkImport`** with `kube-lb0` imports LoadBalancer VIPs into the BGP RIB for advertisement

### Using nodeSelector with Unnumbered Peers

While unnumbered BGP removes the need for per-subnet nodeSelectors, the `nodeSelector` field remains useful when nodes have different interface names. For example, if some nodes use `eth1` and others use `ens192`:

```yaml
neighbors:
  - config:
      neighborInterface: "eth1"
      peerAsn: 64514
    nodeSelector:
      matchLabels:
        network.example.com/interface: "eth1"

  - config:
      neighborInterface: "ens192"
      peerAsn: 64514
    nodeSelector:
      matchLabels:
        network.example.com/interface: "ens192"
```

## FRR Router Configuration

### Important: Point-to-Point Requirement

FRR's `neighbor <interface> interface` command is designed for **point-to-point links only**. From the FRR documentation:

> "This connection type is meant for point to point connections. If you are on an ethernet segment and attempt to use this with more than one bgp neighbor, only one neighbor will come up."

In Kubernetes clusters where multiple nodes share an Ethernet segment (broadcast domain), use addressed peers with `nodeSelector` instead of unnumbered BGP. Unnumbered BGP is appropriate when each node has a dedicated point-to-point link to the router (e.g., in a leaf-spine fabric).

### Point-to-Point Links (Unnumbered)

When each node has a dedicated P2P link to the router:

```
router bgp 64514
  neighbor ens19 interface remote-as 64515
  !
  address-family ipv4 unicast
    neighbor ens19 activate
  exit-address-family
  !
  address-family ipv6 unicast
    neighbor ens19 activate
  exit-address-family
```

### Broadcast Segments (Addressed with Dynamic Neighbors)

When multiple nodes share an Ethernet segment, use addressed peers on k8gobgp with a peer-group and `bgp listen range` on FRR:

```
router bgp 64514
  neighbor k8s peer-group
  neighbor k8s remote-as 64515
  bgp listen range 172.30.250.0/24 peer-group k8s
  bgp listen range 172.30.251.0/24 peer-group k8s
  !
  address-family ipv4 unicast
    neighbor k8s activate
  exit-address-family
  !
  address-family ipv6 unicast
    neighbor k8s activate
  exit-address-family
```

This accepts BGP connections from any node in the specified subnets without requiring per-node configuration on the router.

### Verifying on FRR

```bash
# Check BGP session status
vtysh -c "show bgp summary"

# Check a specific neighbor's capabilities (look for Extended Next-Hop)
vtysh -c "show bgp neighbors ens19"

# Verify received routes
vtysh -c "show bgp ipv4 unicast"
vtysh -c "show bgp ipv6 unicast"
```

## Verifying on k8gobgp

Check the pod logs for successful peering:

```bash
# Look for peer establishment
kubectl logs -n k8gobgp-system -l app=k8gobgp | grep "Peer Up"

# Verify the interface peer was added (not an addressed peer)
kubectl logs -n k8gobgp-system -l app=k8gobgp | grep "Adding neighbor"
```

A successful setup shows:
```
Adding neighbor  {"key": "iface:eth1"}
Peer Up          {"Key":"172.30.250.1"}   # resolved link-local maps back to this
```

Note that GoBGP resolves the link-local address and may log the resolved address in "Peer Up" messages. The controller uses the `iface:eth1` key internally to track interface-based peers, which avoids confusion with the resolved address.

## Comparison: Addressed vs. Unnumbered

| Aspect | Addressed | Unnumbered |
|--------|-----------|------------|
| Neighbor config | IP address per router | Interface name (same on all nodes) |
| Multi-subnet | Requires `nodeSelector` per subnet | Single config for all subnets |
| IP management | Must track router IPs | None — uses link-local auto-discovery |
| IPv4 routing | Direct IPv4 next-hop | Via RFC 5549 Extended Next-Hop |
| Link type | Any (broadcast or P2P) | **Point-to-point only** (FRR limitation) |
| When to use | Shared Ethernet segments, route servers | Dedicated P2P links (leaf-spine fabrics) |
