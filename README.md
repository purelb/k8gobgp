# k8gobgp

[![CI](https://github.com/purelb/k8gobgp/actions/workflows/ci.yaml/badge.svg)](https://github.com/purelb/k8gobgp/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/purelb/k8gobgp)](https://github.com/purelb/k8gobgp/releases)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/purelb/k8gobgp)](https://goreportcard.com/report/github.com/purelb/k8gobgp)

A Kubernetes controller for managing GoBGP configurations using Custom Resource Definitions (CRDs). This project implements comprehensive BGP configuration management through the Kubernetes API, leveraging the [gobgp-netlink](https://github.com/purelb/gobgp-netlink) fork for enhanced Linux kernel integration.

## Features

- **Full BGP Configuration via CRDs**: Manage all GoBGP settings declaratively through Kubernetes
- **Neighbor Management**: Configure BGP peers with full support for timers, authentication, and AFI/SAFI
- **Peer Groups**: Define reusable peer group templates for consistent neighbor configuration
- **Dynamic Neighbors**: Support for dynamic BGP peering with prefix-based matching
- **VRF Support**: Configure Virtual Routing and Forwarding instances
- **Route Policies**: Define import/export policies with prefix lists, community matching, and AS path manipulation
- **Netlink Integration**: Import connected routes from Linux kernel and export BGP routes to routing tables
- **Security**:
  - Secret-based authentication (no plaintext passwords in CRDs)
  - Non-root container execution with minimal capabilities
  - Unix socket communication option for GoBGP
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
# Install CRDs
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/crds.yaml

# Install RBAC
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/rbac.yaml

# Install DaemonSet
kubectl apply -f https://github.com/purelb/k8gobgp/releases/latest/download/daemonset.yaml
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
    routerID: "192.168.1.1"
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

Peer groups allow you to define common settings that are inherited by multiple neighbors:

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

## Configuration Reference

See the [config/samples/](config/samples/) directory for comprehensive examples including:

- Basic neighbor configuration
- Netlink import/export
- Route policies and defined sets
- Peer groups and dynamic neighbors
- Authentication with Secrets

### Important Notes

- **Global Configuration Changes**: Changes to `global.asn` or `global.routerID` require a pod restart to take effect. These are immutable at runtime in GoBGP.
- **Neighbor/Peer Group Changes**: Neighbors, peer groups, policies, and other settings can be updated dynamically without pod restart.

## Architecture

k8gobgp runs as a DaemonSet, with one instance per node. Each instance:

1. Watches for `BGPConfiguration` resources in its namespace
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

## Metrics

The controller exposes Prometheus metrics on `:8080/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `k8gobgp_reconcile_total` | Counter | Total reconciliations by result |
| `k8gobgp_reconcile_duration_seconds` | Histogram | Reconciliation duration |
| `k8gobgp_neighbors_configured` | Gauge | Number of configured neighbors |
| `k8gobgp_neighbors_established` | Gauge | Number of established sessions |
| `k8gobgp_gobgpd_connection_status` | Gauge | GoBGP daemon connection status |
| `k8gobgp_configuration_ready` | Gauge | Configuration ready status |

## Development

### Prerequisites

- Go 1.22+
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

Use the `gobgp` CLI inside the pod to inspect BGP state:

```bash
# Get pod name
POD=$(kubectl -n k8gobgp-system get pod -l app.kubernetes.io/name=k8gobgp -o jsonpath='{.items[0].metadata.name}')

# Check global config
kubectl -n k8gobgp-system exec $POD -- gobgp --target passthrough:///unix:///var/run/gobgp/gobgp.sock global

# List neighbors
kubectl -n k8gobgp-system exec $POD -- gobgp --target passthrough:///unix:///var/run/gobgp/gobgp.sock neighbor

# Show neighbor details
kubectl -n k8gobgp-system exec $POD -- gobgp --target passthrough:///unix:///var/run/gobgp/gobgp.sock neighbor <peer-ip>
```

### Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| BGPConfiguration stuck in not-ready | Global config change | Delete and recreate pod to apply ASN/RouterID changes |
| Neighbor not establishing | Authentication mismatch | Verify Secret exists and key matches |
| No routes imported | Netlink config missing | Check `netlinkImport.enabled` and interface patterns |

## Contributing

Contributions are welcome! Please read our contributing guidelines and submit pull requests to the main repository.

## License

Copyright 2024 Acnodal Inc.

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
- [gobgp-netlink](https://github.com/purelb/gobgp-netlink) - Enhanced GoBGP fork with netlink integration
- [PureLB](https://purelb.io) - Kubernetes load balancer project
