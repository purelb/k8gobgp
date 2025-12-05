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
- **Validation Webhook**: Comprehensive admission validation for configuration errors

## Installation

### Prerequisites

- Kubernetes 1.25+
- [cert-manager](https://cert-manager.io/) (for webhook TLS certificates)

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

### Authentication Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bgp-secrets
  namespace: k8gobgp-system
type: Opaque
stringData:
  upstream-password: "your-md5-password"
```

## Configuration Reference

See the [config/samples/](config/samples/) directory for comprehensive examples including:

- Basic neighbor configuration
- Netlink import/export
- Route policies and defined sets
- Peer groups and dynamic neighbors
- Authentication with Secrets

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
