# Upgrade k8gobgp to use purelb/gobgp-netlink (v4) + Controller Best Practices

## Summary

Upgrade the k8gobgp Kubernetes controller from `github.com/osrg/gobgp/v3` to use the **purelb/gobgp-netlink** fork which provides enhanced netlink integration for Linux kernel route management.

**Fork details:**
- GitHub: https://github.com/purelb/gobgp-netlink
- Local clone: `/home/adamd/go/gobgp`
- Module path: `github.com/osrg/gobgp/v4` (maintains compatibility with upstream)
- Key features: Netlink route import/export, VRF support, IPv6 link-local nexthop handling

## Key Changes Required

### 1. GoBGP API Version Upgrade (v3 → v4 gobgp-netlink fork)

**Current state:**
- go.mod: `github.com/osrg/gobgp/v3 v3.37.0` (upstream)
- Controller imports: `gobgpapi "github.com/osrg/gobgp/v3/api"`
- Has vendored `gobgp_src/` directory (old v3 copy)

**Target state:**
- go.mod: `github.com/osrg/gobgp/v4` with replace directive pointing to purelb fork
- Controller imports: `gobgpapi "github.com/osrg/gobgp/v4/api"`
- Remove `gobgp_src/` directory, use gobgp-netlink fork

**API Changes (v3 → v4 gobgp-netlink):**
- Service renamed: `GobgpApiClient` → `GoBgpServiceClient`
- **New netlink APIs** (fork-specific):
  - `EnableNetlink` - Enable netlink route integration
  - `GetNetlink` - Get netlink configuration status
  - `GetNetlinkImportStats` - Statistics for imported routes
  - `ListNetlinkExport` - List exported routes
  - `GetNetlinkExportStats` - Export statistics
  - `FlushNetlinkExport` - Flush exported routes
  - `ListNetlinkExportRules` - List export rules
- Package remains `api` but module path changes to v4

### 2. Controller Best Practice Fixes

| Issue | Priority | Fix |
|-------|----------|-----|
| No finalizers | HIGH | Add finalizer for BGP cleanup on CR deletion |
| No retry/requeue | HIGH | Add `RequeueAfter` for transient errors |
| Hardcoded gRPC endpoint | HIGH | Make configurable via env var |
| Missing Status Conditions | MEDIUM | Add metav1.Condition array |
| No connection pooling | MEDIUM | Reuse gRPC connection |
| 583-line monolithic file | MEDIUM | Split into focused files |
| Debug printf in main.go | LOW | Remove |

---

## Implementation Plan

### Phase 1: Update Dependencies to Use gobgp-netlink Fork

**Files to modify:**
- [go.mod](go.mod)
- [go.sum](go.sum) (regenerated)

**Steps:**
1. Update go.mod to use the purelb/gobgp-netlink fork:
   ```go
   require (
       github.com/osrg/gobgp/v4 v4.0.0
       // ... other deps
   )

   // Use the purelb/gobgp-netlink fork with netlink enhancements
   // For local development:
   replace github.com/osrg/gobgp/v4 => /home/adamd/go/gobgp
   // For CI/production (when published):
   // replace github.com/osrg/gobgp/v4 => github.com/purelb/gobgp-netlink v4.x.x
   ```

2. Remove `gobgp_src/` directory (no longer needed - using fork directly)

3. Run `go mod tidy`

### Phase 2: Update Import Paths

**Files to modify:**
- [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

**Changes:**
```go
// Before
import gobgpapi "github.com/osrg/gobgp/v3/api"

// After
import gobgpapi "github.com/osrg/gobgp/v4/api"
```

### Phase 3: Update API Client Usage

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

The gRPC client interface renamed from `GobgpApiClient` to `GoBgpServiceClient`:

```go
// Before (line 43)
apiClient := gobgpapi.NewGobgpApiClient(conn)

// After
apiClient := gobgpapi.NewGoBgpServiceClient(conn)
```

### Phase 4: Add Finalizer Support (Best Practice)

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

Add constants and finalizer logic:
```go
const finalizerName = "bgp.purelb.io/finalizer"

func (r *BGPConfigurationReconciler) Reconcile(...) {
    // Handle deletion
    if !bgpConfig.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
            // Cleanup: stop BGP, remove neighbors
            r.cleanupBGPConfiguration(ctx, apiClient, log)
            controllerutil.RemoveFinalizer(bgpConfig, finalizerName)
            return ctrl.Result{}, r.Update(ctx, bgpConfig)
        }
        return ctrl.Result{}, nil
    }

    // Add finalizer if not present
    if !controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
        controllerutil.AddFinalizer(bgpConfig, finalizerName)
        return ctrl.Result{}, r.Update(ctx, bgpConfig)
    }
    // ... rest of reconciliation
}
```

**Add RBAC marker:**
```go
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpconfigurations/finalizers,verbs=update
```

### Phase 5: Add Retry/Requeue Logic (Best Practice)

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

```go
import "time"

const (
    requeueDelay = 30 * time.Second
)

func (r *BGPConfigurationReconciler) Reconcile(...) {
    // On transient errors (connection failures), requeue
    if err := r.reconcileGlobal(...); err != nil {
        log.Error(err, "Failed to reconcile global config, will retry")
        return ctrl.Result{RequeueAfter: requeueDelay}, nil
    }
    // ...
}
```

### Phase 6: Make gRPC Endpoint Configurable

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

```go
type BGPConfigurationReconciler struct {
    client.Client
    Log         logr.Logger
    Scheme      *runtime.Scheme
    GRPCAddress string  // NEW: configurable endpoint
}

func (r *BGPConfigurationReconciler) Reconcile(...) {
    addr := r.GRPCAddress
    if addr == "" {
        addr = "localhost:50051"
    }
    conn, err := grpc.DialContext(ctx, addr, ...)
}
```

**File:** [cmd/manager/main.go](cmd/manager/main.go)

```go
var grpcAddr string
flag.StringVar(&grpcAddr, "gobgp-address", "localhost:50051", "GoBGP gRPC address")

// ...
if err = (&controllers.BGPConfigurationReconciler{
    Client:      mgr.GetClient(),
    Log:         ctrl.Log.WithName("controllers").WithName("BGPConfiguration"),
    Scheme:      mgr.GetScheme(),
    GRPCAddress: grpcAddr,
}).SetupWithManager(mgr); err != nil {
```

### Phase 7: Add Status Conditions (Best Practice)

**File:** [api/v1alpha1/types.go](api/v1alpha1/types.go)

```go
type BGPConfigurationStatus struct {
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    // Netlink status (read from gobgpd)
    NetlinkImportEnabled bool   `json:"netlinkImportEnabled,omitempty"`
    NetlinkExportEnabled bool   `json:"netlinkExportEnabled,omitempty"`
}
```

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

```go
import "k8s.io/apimachinery/pkg/api/meta"

const (
    ConditionTypeReady = "Ready"
    ConditionTypeSynced = "Synced"
)

// After successful reconciliation:
meta.SetStatusCondition(&bgpConfig.Status.Conditions, metav1.Condition{
    Type:    ConditionTypeReady,
    Status:  metav1.ConditionTrue,
    Reason:  "ReconcileSuccess",
    Message: "BGP configuration applied successfully",
})
```

### Phase 7b: Add Netlink CRD Types

**File:** [api/v1alpha1/types.go](api/v1alpha1/types.go)

Add new netlink configuration types to the CRD spec:

```go
// +kubebuilder:object:generate=true
type BGPConfigurationSpec struct {
    Global            GlobalSpec         `json:"global"`
    Neighbors         []Neighbor         `json:"neighbors,omitempty"`
    PeerGroups        []PeerGroup        `json:"peerGroups,omitempty"`
    DynamicNeighbors  []DynamicNeighbor  `json:"dynamicNeighbors,omitempty"`
    Vrfs              []Vrf              `json:"vrfs,omitempty"`
    PolicyDefinitions []PolicyDefinition `json:"policyDefinitions,omitempty"`
    DefinedSets       []DefinedSet       `json:"definedSets,omitempty"`
    // NEW: Netlink configuration (gobgp-netlink fork specific)
    Netlink           *NetlinkConfig     `json:"netlink,omitempty"`
}

// NetlinkConfig configures Linux kernel netlink route integration
// This is specific to the purelb/gobgp-netlink fork
// +kubebuilder:object:generate=true
type NetlinkConfig struct {
    // Enable netlink route import from kernel to BGP
    Enabled bool `json:"enabled,omitempty"`
    // VRF name for netlink operations (empty = default VRF)
    Vrf string `json:"vrf,omitempty"`
    // Network interfaces to monitor for route import
    Interfaces []string `json:"interfaces,omitempty"`
    // Community to attach to imported routes
    Community string `json:"community,omitempty"`
    // List of communities to attach to imported routes
    CommunityList []string `json:"communityList,omitempty"`
    // List of large communities to attach to imported routes
    LargeCommunityList []string `json:"largeCommunityList,omitempty"`
    // Additional VRF imports (for multi-VRF setups)
    VrfImports []NetlinkVrfImport `json:"vrfImports,omitempty"`
}

// NetlinkVrfImport configures netlink import for a specific VRF
// +kubebuilder:object:generate=true
type NetlinkVrfImport struct {
    // VRF name
    VrfName string `json:"vrfName"`
    // Interfaces to monitor in this VRF
    Interfaces []string `json:"interfaces,omitempty"`
}
```

### Phase 7c: Add Netlink Reconciliation

**File:** [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go)

Add new reconciliation function for netlink:

```go
func (r *BGPConfigurationReconciler) reconcileNetlink(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
    if bgpConfig.Spec.Netlink == nil || !bgpConfig.Spec.Netlink.Enabled {
        return nil
    }

    nl := bgpConfig.Spec.Netlink
    req := &gobgpapi.EnableNetlinkRequest{
        Vrf:                nl.Vrf,
        Interfaces:         nl.Interfaces,
        Community:          nl.Community,
        CommunityList:      nl.CommunityList,
        LargeCommunityList: nl.LargeCommunityList,
    }

    log.Info("Enabling netlink integration", "vrf", nl.Vrf, "interfaces", nl.Interfaces)
    _, err := apiClient.EnableNetlink(ctx, req)
    if err != nil {
        log.Error(err, "Failed to enable netlink")
        return err
    }

    // Enable additional VRF imports
    for _, vrfImport := range nl.VrfImports {
        req := &gobgpapi.EnableNetlinkRequest{
            Vrf:        vrfImport.VrfName,
            Interfaces: vrfImport.Interfaces,
        }
        if _, err := apiClient.EnableNetlink(ctx, req); err != nil {
            log.Error(err, "Failed to enable netlink for VRF", "vrf", vrfImport.VrfName)
        }
    }

    return nil
}
```

Add to main Reconcile function:
```go
if err := r.reconcileNetlink(ctx, apiClient, bgpConfig, log); err != nil {
    return ctrl.Result{RequeueAfter: requeueDelay}, nil
}
```

### Phase 8: Change API Group to bgp.purelb.io and Version to v1

**Directory rename:**
- `api/v1alpha1/` → `api/v1/`

**Files to modify:**
- [api/v1/scheme.go](api/v1/scheme.go) (renamed from v1alpha1)
- [api/v1/types.go](api/v1/types.go) (renamed from v1alpha1)
- [config/crd/bases/](config/crd/bases/) → CRD regenerated as `bgp.purelb.io_bgpconfigurations.yaml`
- [config/rbac/role.yaml](config/rbac/role.yaml)
- [controllers/bgpconfiguration_controller.go](controllers/bgpconfiguration_controller.go) - update import path
- [cmd/manager/main.go](cmd/manager/main.go) - update import path

**File:** [api/v1/scheme.go](api/v1/scheme.go)

```go
package v1  // Changed from v1alpha1

var (
    // GroupVersion is the group version used to register these objects
    GroupVersion = schema.GroupVersion{Group: "bgp.purelb.io", Version: "v1"}
    // ...
)
```

**File:** [api/v1/types.go](api/v1/types.go)

```go
package v1  // Changed from v1alpha1
```

**Update imports in controller and main.go:**
```go
// Before
import gobgpk8siov1alpha1 "github.com/adamd/k8gobgp/api/v1alpha1"

// After
import bgpv1 "github.com/adamd/k8gobgp/api/v1"
```

**Update RBAC markers in controller:**
```go
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpconfigurations/finalizers,verbs=update
```

### Phase 9: Cleanup & Code Quality

**File:** [cmd/manager/main.go](cmd/manager/main.go)
- Remove debug line: `fmt.Printf("Registered types: %+v\n", ...)`
- Uncomment: `LeaderElectionReleaseOnCancel: true`
- Remove unused `fmt` import

### Phase 10: Update Dockerfile for gobgp-netlink

**File:** [Dockerfile](Dockerfile)

**Strategy:** Use `go mod vendor` to bundle all dependencies (including gobgp-netlink) into the build context. This avoids needing multi-repo Docker contexts.

```dockerfile
FROM golang:1.24-alpine AS gobgpd_builder
WORKDIR /gobgp_app
# Build gobgpd from the purelb/gobgp-netlink fork
# Option A: Clone from GitHub
RUN apk add --no-cache git
RUN git clone --depth 1 https://github.com/purelb/gobgp-netlink.git .
RUN go build -o gobgpd ./cmd/gobgpd
RUN go build -o gobgp ./cmd/gobgp

FROM golang:1.24-alpine AS reconciler_builder
WORKDIR /k8gobgp_app
COPY . .
# Vendor includes gobgp-netlink via replace directive
RUN go build -mod=vendor -o manager ./cmd/manager

FROM alpine:latest
WORKDIR /
COPY --from=gobgpd_builder /gobgp_app/gobgpd /usr/local/bin/gobgpd
COPY --from=gobgpd_builder /gobgp_app/gobgp /usr/local/bin/gobgp
COPY --from=reconciler_builder /k8gobgp_app/manager /usr/local/bin/manager
EXPOSE 50051
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
```

**Pre-build steps (run locally before docker build):**
```bash
# Vendor dependencies including gobgp-netlink fork
go mod vendor
```

### Phase 11: Regenerate & Test

```bash
# Regenerate CRD and deepcopy
make generate
make manifests

# Build and test
go build ./...
go test ./...

# Build container
docker build -t ghcr.io/adamdunstan/registry/k8gobgp:latest .
```

---

## Files Modified Summary

| File | Changes |
|------|---------|
| `go.mod` | Update gobgp v3→v4 (purelb fork), add replace directive |
| `go.sum` | Regenerated |
| `controllers/bgpconfiguration_controller.go` | Import path v3→v4, client `GobgpApiClient`→`GoBgpServiceClient`, import `api/v1`, finalizers, requeue logic, configurable endpoint, netlink reconciliation |
| `api/v1alpha1/` | **RENAME** to `api/v1/` |
| `api/v1/types.go` | Package `v1`, add `NetlinkConfig` types, add `Conditions` to Status |
| `api/v1/scheme.go` | Package `v1`, API group `bgp.purelb.io`, version `v1` |
| `cmd/manager/main.go` | Import `api/v1`, remove debug printf, add `--gobgp-address` flag, enable leader election cleanup |
| `config/crd/bases/` | Regenerated as `bgp.purelb.io_bgpconfigurations.yaml` |
| `config/rbac/role.yaml` | Updated for `bgp.purelb.io` group |
| `Dockerfile` | Clone gobgp-netlink from GitHub, vendor-based build |
| `gobgp_src/` | **DELETE** (no longer needed) |

---

## User Decisions

1. **Scope**: **Full** - All best practices fixes including Status Conditions, code refactoring
2. **API group**: Change to `bgp.purelb.io`
3. **API version**: Change from `v1alpha1` to `v1`
4. **Netlink CRD**: Yes - Add CRD fields for all netlink functions

---

## Engineering Review - Gaps & Additional Items

### Issues Identified & Resolutions

| Issue | Severity | Resolution |
|-------|----------|------------|
| **Sample YAML uses old API group/version** | HIGH | Add Phase 12: Update `config/samples/*.yaml` to `bgp.purelb.io/v1` |
| **cleanupBGPConfiguration not defined** | HIGH | Add implementation in Phase 4 |
| **gRPC connection timeout missing** | MEDIUM | Add timeout in Phase 6 |
| **entrypoint.sh - no health checks** | MEDIUM | Add Phase 13: Improve entrypoint |
| **DaemonSet YAML - no resource limits/probes** | MEDIUM | Add Phase 14: Update DaemonSet |
| **No printer columns in CRD** | LOW | Add kubebuilder markers in Phase 7 |
| **hack/boilerplate.go.txt may be missing** | LOW | Verify exists before `make generate` |

### Additional Phases Required

#### Phase 12: Update Sample YAML Files

**File:** `config/samples/bgpconfiguration-neighbor-sample.yaml`
```yaml
# Before
apiVersion: bgp.example.com/v1alpha1

# After
apiVersion: bgp.purelb.io/v1
```

#### Phase 13: Improve entrypoint.sh

**File:** `entrypoint.sh`
```bash
#!/bin/sh
set -e

# Start gobgpd in background
/usr/local/bin/gobgpd &
GOBGPD_PID=$!

# Wait for gRPC to be ready (up to 30 seconds)
echo "Waiting for gobgpd gRPC server..."
for i in $(seq 1 30); do
    if /usr/local/bin/gobgp global 2>/dev/null; then
        echo "gobgpd is ready"
        break
    fi
    sleep 1
done

# Start the controller manager
exec /usr/local/bin/manager "$@"
```

#### Phase 14: Update DaemonSet with Best Practices

**File:** `config/daemonset/gobgp-daemonset.yaml`

Add resource limits and health probes:
```yaml
containers:
- name: gobgp
  image: ghcr.io/adamdunstan/registry/k8gobgp:latest
  resources:
    requests:
      memory: "64Mi"
      cpu: "100m"
    limits:
      memory: "256Mi"
      cpu: "500m"
  livenessProbe:
    httpGet:
      path: /healthz
      port: 8081
    initialDelaySeconds: 15
    periodSeconds: 20
  readinessProbe:
    httpGet:
      path: /readyz
      port: 8081
    initialDelaySeconds: 5
    periodSeconds: 10
```

### Missing Implementation Details

**1. cleanupBGPConfiguration function (add to Phase 4):**
```go
func (r *BGPConfigurationReconciler) cleanupBGPConfiguration(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, log logr.Logger) error {
    log.Info("Cleaning up BGP configuration")
    _, err := apiClient.StopBgp(ctx, &gobgpapi.StopBgpRequest{})
    if err != nil {
        log.Error(err, "Failed to stop BGP during cleanup")
    }
    return nil // Continue even if stop fails
}
```

**2. gRPC connection with timeout (update Phase 6):**
```go
// Add timeout for gRPC connection
dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()

conn, err := grpc.DialContext(dialCtx, r.GRPCAddress,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
    grpc.WithBlock(),
)
```

**3. Printer columns (add to Phase 7 types.go):**
```go
// +kubebuilder:printcolumn:name="ASN",type=integer,JSONPath=`.spec.global.asn`
// +kubebuilder:printcolumn:name="RouterID",type=string,JSONPath=`.spec.global.routerID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BGPConfiguration struct {
```

### Recommended Execution Order

To minimize risk, execute phases in this order:

1. **Phase 8** - Directory rename `api/v1alpha1/` → `api/v1/` (do first to avoid import conflicts)
2. **Phase 1** - Update go.mod, delete `gobgp_src/`
3. **Phase 2-3** - Update imports and API client name
4. **Run `go mod tidy`** - Verify dependencies resolve
5. **Phase 7, 7b** - Add types (Status Conditions, Netlink)
6. **Run `make generate && make manifests`** - Regenerate before controller changes
7. **Phase 4-6, 7c** - Controller logic (finalizers, requeue, configurable endpoint, netlink)
8. **Phase 9** - Cleanup main.go
9. **Phase 10** - Dockerfile
10. **Phase 12-14** - Config files (samples, entrypoint, daemonset)
11. **Phase 11** - Final build and test

### Files Summary (Complete)

| File | Changes |
|------|---------|
| `go.mod` | Update gobgp v3→v4 (purelb fork), add replace directive |
| `go.sum` | Regenerated |
| `api/v1alpha1/` | **RENAME** → `api/v1/` |
| `api/v1/types.go` | Package `v1`, NetlinkConfig, Conditions, printer columns |
| `api/v1/scheme.go` | Package `v1`, group `bgp.purelb.io`, version `v1` |
| `api/v1/zz_generated.deepcopy.go` | Regenerated |
| `controllers/bgpconfiguration_controller.go` | v4 imports, GoBgpServiceClient, finalizers, requeue, configurable endpoint, netlink, cleanup function |
| `cmd/manager/main.go` | v1 import, remove debug, add flag, leader election |
| `config/crd/bases/` | Regenerated `bgp.purelb.io_bgpconfigurations.yaml` |
| `config/rbac/role.yaml` | Regenerated for `bgp.purelb.io` |
| `config/samples/*.yaml` | Update apiVersion to `bgp.purelb.io/v1` |
| `config/daemonset/gobgp-daemonset.yaml` | Add resources, probes |
| `entrypoint.sh` | Add health check wait loop |
| `Dockerfile` | Clone gobgp-netlink, vendor build |
| `gobgp_src/` | **DELETE** |

---

## Phase 15: Unit Tests for Controller

**New file:** `controllers/bgpconfiguration_controller_test.go`

### Mock gRPC Client

```go
package controllers

import (
    "context"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/mock"
    "google.golang.org/grpc"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
    "sigs.k8s.io/controller-runtime/pkg/reconcile"

    bgpv1 "github.com/adamd/k8gobgp/api/v1"
    gobgpapi "github.com/osrg/gobgp/v4/api"
)

// MockGoBgpServiceClient implements gobgpapi.GoBgpServiceClient for testing
type MockGoBgpServiceClient struct {
    mock.Mock
}

func (m *MockGoBgpServiceClient) StartBgp(ctx context.Context, req *gobgpapi.StartBgpRequest, opts ...grpc.CallOption) (*gobgpapi.StartBgpResponse, error) {
    args := m.Called(ctx, req)
    return args.Get(0).(*gobgpapi.StartBgpResponse), args.Error(1)
}

func (m *MockGoBgpServiceClient) StopBgp(ctx context.Context, req *gobgpapi.StopBgpRequest, opts ...grpc.CallOption) (*gobgpapi.StopBgpResponse, error) {
    args := m.Called(ctx, req)
    return args.Get(0).(*gobgpapi.StopBgpResponse), args.Error(1)
}

func (m *MockGoBgpServiceClient) GetBgp(ctx context.Context, req *gobgpapi.GetBgpRequest, opts ...grpc.CallOption) (*gobgpapi.GetBgpResponse, error) {
    args := m.Called(ctx, req)
    if args.Get(0) == nil {
        return nil, args.Error(1)
    }
    return args.Get(0).(*gobgpapi.GetBgpResponse), args.Error(1)
}

// ... implement other methods as needed
```

### Test Cases

```go
func TestReconcile_CreateNewBGPConfig(t *testing.T) {
    scheme := runtime.NewScheme()
    _ = bgpv1.AddToScheme(scheme)

    bgpConfig := &bgpv1.BGPConfiguration{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "test-config",
            Namespace: "default",
        },
        Spec: bgpv1.BGPConfigurationSpec{
            Global: bgpv1.GlobalSpec{
                ASN:      64512,
                RouterID: "192.168.1.1",
            },
        },
    }

    client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bgpConfig).Build()

    r := &BGPConfigurationReconciler{
        Client:      client,
        Scheme:      scheme,
        GRPCAddress: "localhost:50051",
    }

    // Test reconciliation
    req := reconcile.Request{
        NamespacedName: types.NamespacedName{
            Name:      "test-config",
            Namespace: "default",
        },
    }

    // Note: Full test requires mock gRPC server or interface abstraction
}

func TestReconcile_Finalizer_Added(t *testing.T) {
    // Test that finalizer is added to new resources
}

func TestReconcile_Finalizer_Cleanup(t *testing.T) {
    // Test that cleanup is performed when resource is deleted
}

func TestReconcile_RequeueOnError(t *testing.T) {
    // Test that transient errors cause requeue
}

func TestCrdToAPIGlobal(t *testing.T) {
    crd := &bgpv1.GlobalSpec{
        ASN:      64512,
        RouterID: "192.168.1.1",
        ListenPort: 179,
    }

    result := crdToAPIGlobal(crd)

    assert.Equal(t, uint32(64512), result.Asn)
    assert.Equal(t, "192.168.1.1", result.RouterId)
    assert.Equal(t, int32(179), result.ListenPort)
}

func TestCrdToAPINeighbor(t *testing.T) {
    // Test neighbor conversion
}

func TestCrdToAPINetlinkConfig(t *testing.T) {
    // Test netlink config conversion
}
```

### Refactor for Testability

To make the controller more testable, extract gRPC client creation into an interface:

**File:** `controllers/grpc_client.go`
```go
package controllers

import (
    "context"
    gobgpapi "github.com/osrg/gobgp/v4/api"
)

// GoBgpClient interface for mocking
type GoBgpClient interface {
    gobgpapi.GoBgpServiceClient
}

// GoBgpClientFactory creates gRPC clients
type GoBgpClientFactory interface {
    NewClient(ctx context.Context, address string) (GoBgpClient, func(), error)
}

// DefaultGoBgpClientFactory implements GoBgpClientFactory
type DefaultGoBgpClientFactory struct{}

func (f *DefaultGoBgpClientFactory) NewClient(ctx context.Context, address string) (GoBgpClient, func(), error) {
    conn, err := grpc.DialContext(ctx, address,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithBlock(),
    )
    if err != nil {
        return nil, nil, err
    }
    cleanup := func() { conn.Close() }
    return gobgpapi.NewGoBgpServiceClient(conn), cleanup, nil
}
```

Update reconciler to use factory:
```go
type BGPConfigurationReconciler struct {
    client.Client
    Log           logr.Logger
    Scheme        *runtime.Scheme
    GRPCAddress   string
    ClientFactory GoBgpClientFactory  // NEW: injectable for testing
}
```

---

## Phase 16: Integration Tests

**New directory:** `test/integration/`

### Setup Test Environment

**File:** `test/integration/suite_test.go`
```go
package integration

import (
    "context"
    "path/filepath"
    "testing"
    "time"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/rest"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/envtest"
    logf "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/log/zap"

    bgpv1 "github.com/adamd/k8gobgp/api/v1"
    "github.com/adamd/k8gobgp/controllers"
)

var (
    cfg       *rest.Config
    k8sClient client.Client
    testEnv   *envtest.Environment
    ctx       context.Context
    cancel    context.CancelFunc
)

func TestIntegration(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "Controller Integration Suite")
}

var _ = BeforeSuite(func() {
    logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

    ctx, cancel = context.WithCancel(context.Background())

    By("bootstrapping test environment")
    testEnv = &envtest.Environment{
        CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
        ErrorIfCRDPathMissing: true,
    }

    var err error
    cfg, err = testEnv.Start()
    Expect(err).NotTo(HaveOccurred())
    Expect(cfg).NotTo(BeNil())

    err = bgpv1.AddToScheme(scheme.Scheme)
    Expect(err).NotTo(HaveOccurred())

    k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
    Expect(err).NotTo(HaveOccurred())
    Expect(k8sClient).NotTo(BeNil())

    // Start controller manager
    k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
        Scheme: scheme.Scheme,
    })
    Expect(err).ToNot(HaveOccurred())

    err = (&controllers.BGPConfigurationReconciler{
        Client:      k8sManager.GetClient(),
        Scheme:      k8sManager.GetScheme(),
        GRPCAddress: "localhost:50051", // Mock or skip for integration tests
    }).SetupWithManager(k8sManager)
    Expect(err).ToNot(HaveOccurred())

    go func() {
        defer GinkgoRecover()
        err = k8sManager.Start(ctx)
        Expect(err).ToNot(HaveOccurred())
    }()
})

var _ = AfterSuite(func() {
    cancel()
    By("tearing down the test environment")
    err := testEnv.Stop()
    Expect(err).NotTo(HaveOccurred())
})
```

### Integration Test Cases

**File:** `test/integration/bgpconfiguration_test.go`
```go
package integration

import (
    "time"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"

    bgpv1 "github.com/adamd/k8gobgp/api/v1"
)

var _ = Describe("BGPConfiguration Controller", func() {
    const (
        timeout  = time.Second * 10
        interval = time.Millisecond * 250
    )

    Context("When creating a BGPConfiguration", func() {
        It("Should add finalizer", func() {
            bgpConfig := &bgpv1.BGPConfiguration{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "test-bgp-config",
                    Namespace: "default",
                },
                Spec: bgpv1.BGPConfigurationSpec{
                    Global: bgpv1.GlobalSpec{
                        ASN:      64512,
                        RouterID: "192.168.1.1",
                    },
                },
            }

            Expect(k8sClient.Create(ctx, bgpConfig)).Should(Succeed())

            Eventually(func() bool {
                err := k8sClient.Get(ctx, types.NamespacedName{
                    Name:      "test-bgp-config",
                    Namespace: "default",
                }, bgpConfig)
                if err != nil {
                    return false
                }
                return len(bgpConfig.Finalizers) > 0
            }, timeout, interval).Should(BeTrue())
        })

        It("Should update status conditions", func() {
            // Test status condition updates
        })
    })

    Context("When updating a BGPConfiguration", func() {
        It("Should reconcile changes", func() {
            // Test update reconciliation
        })
    })

    Context("When deleting a BGPConfiguration", func() {
        It("Should cleanup and remove finalizer", func() {
            // Test deletion cleanup
        })
    })
})
```

### Update go.mod for Test Dependencies

```go
require (
    // ... existing deps
    github.com/onsi/ginkgo/v2 v2.x.x
    github.com/onsi/gomega v1.x.x
    github.com/stretchr/testify v1.x.x
    sigs.k8s.io/controller-runtime v0.x.x // includes envtest
)
```

### Update Makefile

```makefile
.PHONY: test
test: ## Run unit tests
	go test ./controllers/... -coverprofile cover.out

.PHONY: test-integration
test-integration: ## Run integration tests (requires envtest)
	go test ./test/integration/... -v

.PHONY: envtest
envtest: ## Download envtest-setup locally if necessary
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
```

---

## Phase 17: Prometheus Metrics Integration

### Add Custom Metrics

**New file:** `controllers/metrics.go`
```go
package controllers

import (
    "github.com/prometheus/client_golang/prometheus"
    "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
    // Reconciliation metrics
    reconcileTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "bgp_reconcile_total",
            Help: "Total number of reconciliations per BGPConfiguration",
        },
        []string{"name", "namespace", "result"},
    )

    reconcileDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "bgp_reconcile_duration_seconds",
            Help:    "Duration of reconciliation in seconds",
            Buckets: prometheus.DefBuckets,
        },
        []string{"name", "namespace"},
    )

    // BGP state metrics
    bgpNeighborCount = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "bgp_neighbor_count",
            Help: "Number of configured BGP neighbors",
        },
        []string{"name", "namespace"},
    )

    bgpNeighborState = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "bgp_neighbor_state",
            Help: "BGP neighbor state (1=established, 0=other)",
        },
        []string{"name", "namespace", "neighbor_address", "peer_asn"},
    )

    bgpPrefixCount = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "bgp_prefix_count",
            Help: "Number of prefixes received/advertised",
        },
        []string{"name", "namespace", "direction", "family"},
    )

    // Netlink metrics (fork-specific)
    netlinkImportedRoutes = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "bgp_netlink_imported_routes",
            Help: "Number of routes imported from netlink",
        },
        []string{"name", "namespace", "vrf"},
    )

    netlinkExportedRoutes = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "bgp_netlink_exported_routes",
            Help: "Number of routes exported to netlink",
        },
        []string{"name", "namespace", "vrf"},
    )

    // Error metrics
    grpcErrors = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "bgp_grpc_errors_total",
            Help: "Total gRPC errors communicating with gobgpd",
        },
        []string{"name", "namespace", "operation"},
    )
)

func init() {
    // Register custom metrics with controller-runtime
    metrics.Registry.MustRegister(
        reconcileTotal,
        reconcileDuration,
        bgpNeighborCount,
        bgpNeighborState,
        bgpPrefixCount,
        netlinkImportedRoutes,
        netlinkExportedRoutes,
        grpcErrors,
    )
}
```

### Instrument Reconciler

**Update:** `controllers/bgpconfiguration_controller.go`
```go
import (
    "time"
    "github.com/prometheus/client_golang/prometheus"
)

func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    startTime := time.Now()
    defer func() {
        reconcileDuration.WithLabelValues(req.Name, req.Namespace).Observe(time.Since(startTime).Seconds())
    }()

    // ... existing reconciliation logic ...

    // On success
    reconcileTotal.WithLabelValues(req.Name, req.Namespace, "success").Inc()

    // Update neighbor count metric
    bgpNeighborCount.WithLabelValues(req.Name, req.Namespace).Set(float64(len(bgpConfig.Spec.Neighbors)))

    return ctrl.Result{}, nil
}

// Add to reconcileNeighbors
func (r *BGPConfigurationReconciler) reconcileNeighbors(...) error {
    // After successful neighbor list
    for addr, peer := range currentNeighbors {
        state := 0.0
        if peer.State != nil && peer.State.SessionState == gobgpapi.PeerState_ESTABLISHED {
            state = 1.0
        }
        bgpNeighborState.WithLabelValues(
            bgpConfig.Name,
            bgpConfig.Namespace,
            addr,
            fmt.Sprintf("%d", peer.Conf.PeerAsn),
        ).Set(state)
    }
    // ...
}

// Add to reconcileNetlink
func (r *BGPConfigurationReconciler) reconcileNetlink(...) error {
    // After enabling netlink, get stats
    stats, err := apiClient.GetNetlinkImportStats(ctx, &gobgpapi.GetNetlinkImportStatsRequest{})
    if err == nil {
        netlinkImportedRoutes.WithLabelValues(
            bgpConfig.Name,
            bgpConfig.Namespace,
            bgpConfig.Spec.Netlink.Vrf,
        ).Set(float64(stats.RouteCount))
    }
    // ...
}
```

### Add ServiceMonitor for Prometheus Operator

**New file:** `config/prometheus/servicemonitor.yaml`
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: k8gobgp-controller
  namespace: gobgp-system
  labels:
    app: gobgp
spec:
  selector:
    matchLabels:
      app: gobgp
  endpoints:
  - port: metrics
    interval: 30s
    path: /metrics
```

**Update DaemonSet** to expose metrics port:
```yaml
ports:
- containerPort: 8080
  name: metrics
  protocol: TCP
- containerPort: 50051
  hostPort: 50051
  name: gobgp-grpc
```

---

## Phase 18: Webhook Validation

### Generate Webhook Scaffolding

**New file:** `api/v1/bgpconfiguration_webhook.go`
```go
package v1

import (
    "fmt"
    "net"

    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    logf "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/webhook"
    "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var bgpconfigurationlog = logf.Log.WithName("bgpconfiguration-resource")

func (r *BGPConfiguration) SetupWebhookWithManager(mgr ctrl.Manager) error {
    return ctrl.NewWebhookManagedBy(mgr).
        For(r).
        Complete()
}

// +kubebuilder:webhook:path=/mutate-bgp-purelb-io-v1-bgpconfiguration,mutating=true,failurePolicy=fail,sideEffects=None,groups=bgp.purelb.io,resources=bgpconfigurations,verbs=create;update,versions=v1,name=mbgpconfiguration.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &BGPConfiguration{}

// Default implements webhook.Defaulter
func (r *BGPConfiguration) Default() {
    bgpconfigurationlog.Info("default", "name", r.Name)

    // Set default listen port if not specified
    if r.Spec.Global.ListenPort == 0 {
        r.Spec.Global.ListenPort = 179
    }

    // Set default listen addresses if not specified
    if len(r.Spec.Global.ListenAddresses) == 0 {
        r.Spec.Global.ListenAddresses = []string{"0.0.0.0", "::"}
    }

    // Default neighbor timers
    for i := range r.Spec.Neighbors {
        if r.Spec.Neighbors[i].Timers == nil {
            r.Spec.Neighbors[i].Timers = &Timers{
                Config: TimersConfig{
                    HoldTime:          90,
                    KeepaliveInterval: 30,
                },
            }
        }
    }
}

// +kubebuilder:webhook:path=/validate-bgp-purelb-io-v1-bgpconfiguration,mutating=false,failurePolicy=fail,sideEffects=None,groups=bgp.purelb.io,resources=bgpconfigurations,verbs=create;update,versions=v1,name=vbgpconfiguration.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &BGPConfiguration{}

// ValidateCreate implements webhook.Validator
func (r *BGPConfiguration) ValidateCreate() (admission.Warnings, error) {
    bgpconfigurationlog.Info("validate create", "name", r.Name)
    return r.validateBGPConfiguration()
}

// ValidateUpdate implements webhook.Validator
func (r *BGPConfiguration) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
    bgpconfigurationlog.Info("validate update", "name", r.Name)
    return r.validateBGPConfiguration()
}

// ValidateDelete implements webhook.Validator
func (r *BGPConfiguration) ValidateDelete() (admission.Warnings, error) {
    bgpconfigurationlog.Info("validate delete", "name", r.Name)
    return nil, nil
}

func (r *BGPConfiguration) validateBGPConfiguration() (admission.Warnings, error) {
    var warnings admission.Warnings
    var allErrs []string

    // Validate ASN
    if r.Spec.Global.ASN == 0 {
        allErrs = append(allErrs, "spec.global.asn: must be greater than 0")
    }
    if r.Spec.Global.ASN > 4294967295 {
        allErrs = append(allErrs, "spec.global.asn: must be a valid 32-bit ASN")
    }

    // Validate Router ID (must be valid IPv4)
    if r.Spec.Global.RouterID == "" {
        allErrs = append(allErrs, "spec.global.routerID: is required")
    } else if ip := net.ParseIP(r.Spec.Global.RouterID); ip == nil || ip.To4() == nil {
        allErrs = append(allErrs, "spec.global.routerID: must be a valid IPv4 address")
    }

    // Validate listen port
    if r.Spec.Global.ListenPort < 0 || r.Spec.Global.ListenPort > 65535 {
        allErrs = append(allErrs, "spec.global.listenPort: must be between 0 and 65535")
    }
    if r.Spec.Global.ListenPort > 0 && r.Spec.Global.ListenPort < 1024 {
        warnings = append(warnings, "spec.global.listenPort: using privileged port (<1024) requires elevated permissions")
    }

    // Validate neighbors
    neighborAddrs := make(map[string]bool)
    for i, n := range r.Spec.Neighbors {
        prefix := fmt.Sprintf("spec.neighbors[%d]", i)

        // Check neighbor address
        if n.Config.NeighborAddress == "" {
            allErrs = append(allErrs, fmt.Sprintf("%s.config.neighborAddress: is required", prefix))
        } else {
            if ip := net.ParseIP(n.Config.NeighborAddress); ip == nil {
                allErrs = append(allErrs, fmt.Sprintf("%s.config.neighborAddress: must be a valid IP address", prefix))
            }
            if neighborAddrs[n.Config.NeighborAddress] {
                allErrs = append(allErrs, fmt.Sprintf("%s.config.neighborAddress: duplicate neighbor address", prefix))
            }
            neighborAddrs[n.Config.NeighborAddress] = true
        }

        // Check peer ASN
        if n.Config.PeerAsn == 0 {
            allErrs = append(allErrs, fmt.Sprintf("%s.config.peerAsn: must be greater than 0", prefix))
        }
    }

    // Validate netlink config
    if r.Spec.Netlink != nil && r.Spec.Netlink.Enabled {
        if len(r.Spec.Netlink.Interfaces) == 0 && r.Spec.Netlink.Vrf == "" {
            warnings = append(warnings, "spec.netlink: enabled but no interfaces or VRF specified")
        }
    }

    if len(allErrs) > 0 {
        return warnings, fmt.Errorf("validation failed: %v", allErrs)
    }

    return warnings, nil
}
```

### Update main.go for Webhooks

**File:** `cmd/manager/main.go`
```go
// Add after controller setup
if os.Getenv("ENABLE_WEBHOOKS") != "false" {
    if err = (&bgpv1.BGPConfiguration{}).SetupWebhookWithManager(mgr); err != nil {
        setupLog.Error(err, "unable to create webhook", "webhook", "BGPConfiguration")
        os.Exit(1)
    }
}
```

### Webhook Certificate Management

**New file:** `config/webhook/manifests.yaml`
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: bgp-mutating-webhook
  annotations:
    cert-manager.io/inject-ca-from: gobgp-system/gobgp-serving-cert
webhooks:
- name: mbgpconfiguration.kb.io
  clientConfig:
    service:
      name: gobgp-webhook-service
      namespace: gobgp-system
      path: /mutate-bgp-purelb-io-v1-bgpconfiguration
  rules:
  - apiGroups: ["bgp.purelb.io"]
    apiVersions: ["v1"]
    operations: ["CREATE", "UPDATE"]
    resources: ["bgpconfigurations"]
  sideEffects: None
  admissionReviewVersions: ["v1"]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: bgp-validating-webhook
  annotations:
    cert-manager.io/inject-ca-from: gobgp-system/gobgp-serving-cert
webhooks:
- name: vbgpconfiguration.kb.io
  clientConfig:
    service:
      name: gobgp-webhook-service
      namespace: gobgp-system
      path: /validate-bgp-purelb-io-v1-bgpconfiguration
  rules:
  - apiGroups: ["bgp.purelb.io"]
    apiVersions: ["v1"]
    operations: ["CREATE", "UPDATE"]
    resources: ["bgpconfigurations"]
  sideEffects: None
  admissionReviewVersions: ["v1"]
```

### Webhook Service

**New file:** `config/webhook/service.yaml`
```yaml
apiVersion: v1
kind: Service
metadata:
  name: gobgp-webhook-service
  namespace: gobgp-system
spec:
  ports:
  - port: 443
    targetPort: 9443
  selector:
    app: gobgp
```

### Certificate (using cert-manager)

**New file:** `config/webhook/certificate.yaml`
```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: gobgp-serving-cert
  namespace: gobgp-system
spec:
  dnsNames:
  - gobgp-webhook-service.gobgp-system.svc
  - gobgp-webhook-service.gobgp-system.svc.cluster.local
  issuerRef:
    kind: Issuer
    name: gobgp-selfsigned-issuer
  secretName: gobgp-webhook-server-cert
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: gobgp-selfsigned-issuer
  namespace: gobgp-system
spec:
  selfSigned: {}
```

---

## Updated Execution Order (Complete)

| Step | Phase | Description |
|------|-------|-------------|
| 1 | Phase 8 | Rename `api/v1alpha1/` → `api/v1/`, update scheme |
| 2 | Phase 1 | Update go.mod, delete `gobgp_src/` |
| 3 | Phase 2-3 | Update imports and API client |
| 4 | - | Run `go mod tidy` |
| 5 | Phase 7, 7b | Add types (Status, Netlink) |
| 6 | - | Run `make generate && make manifests` |
| 7 | Phase 4-6, 7c | Controller logic changes |
| 8 | Phase 9 | Cleanup main.go |
| 9 | Phase 10 | Update Dockerfile |
| 10 | Phase 12-14 | Config files (samples, entrypoint, daemonset) |
| 11 | Phase 15 | Unit tests |
| 12 | Phase 16 | Integration tests |
| 13 | Phase 17 | Prometheus metrics |
| 14 | Phase 18 | Webhook validation |
| 15 | Phase 11 | Final build and test |

---

## Complete Files Summary

| File | Changes |
|------|---------|
| `go.mod` | Update gobgp v3→v4, add test deps (ginkgo, gomega, testify) |
| `go.sum` | Regenerated |
| `api/v1alpha1/` | **RENAME** → `api/v1/` |
| `api/v1/types.go` | Package `v1`, NetlinkConfig, Conditions, printer columns |
| `api/v1/scheme.go` | Package `v1`, group `bgp.purelb.io`, version `v1` |
| `api/v1/bgpconfiguration_webhook.go` | **NEW** - Validation and defaulting webhooks |
| `api/v1/zz_generated.deepcopy.go` | Regenerated |
| `controllers/bgpconfiguration_controller.go` | v4 imports, finalizers, requeue, metrics instrumentation |
| `controllers/bgpconfiguration_controller_test.go` | **NEW** - Unit tests |
| `controllers/grpc_client.go` | **NEW** - Interface for testability |
| `controllers/metrics.go` | **NEW** - Prometheus metrics |
| `test/integration/suite_test.go` | **NEW** - Integration test setup |
| `test/integration/bgpconfiguration_test.go` | **NEW** - Integration tests |
| `cmd/manager/main.go` | v1 import, webhook setup, remove debug |
| `config/crd/bases/` | Regenerated `bgp.purelb.io_bgpconfigurations.yaml` |
| `config/rbac/role.yaml` | Regenerated for `bgp.purelb.io` |
| `config/samples/*.yaml` | Update apiVersion to `bgp.purelb.io/v1` |
| `config/daemonset/gobgp-daemonset.yaml` | Add resources, probes, metrics port |
| `config/prometheus/servicemonitor.yaml` | **NEW** - Prometheus ServiceMonitor |
| `config/webhook/manifests.yaml` | **NEW** - Webhook configurations |
| `config/webhook/service.yaml` | **NEW** - Webhook service |
| `config/webhook/certificate.yaml` | **NEW** - cert-manager certificate |
| `entrypoint.sh` | Add health check wait loop |
| `Dockerfile` | Clone gobgp-netlink, vendor build |
| `Makefile` | Add test, test-integration targets |
| `gobgp_src/` | **DELETE** |

---

## Phase 19: GitHub Actions CI/CD Pipeline

### Workflow Overview

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | Push/PR to main | Build, test, lint |
| `release.yaml` | Tag push (v*) | Build & push container, create release |

### CI Workflow

**New file:** `.github/workflows/ci.yaml`
```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

env:
  GO_VERSION: '1.24'

jobs:
  lint:
    name: Lint
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: golangci-lint
        uses: golangci/golangci-lint-action@v4
        with:
          version: latest
          args: --timeout=5m

  test:
    name: Test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Clone gobgp-netlink fork
        run: |
          git clone --depth 1 https://github.com/purelb/gobgp-netlink.git /tmp/gobgp
          echo "replace github.com/osrg/gobgp/v4 => /tmp/gobgp" >> go.mod

      - name: Download dependencies
        run: go mod download

      - name: Run unit tests
        run: go test -v -race -coverprofile=coverage.out ./controllers/...

      - name: Upload coverage
        uses: codecov/codecov-action@v4
        with:
          files: ./coverage.out
          fail_ci_if_error: false

  integration-test:
    name: Integration Tests
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Clone gobgp-netlink fork
        run: |
          git clone --depth 1 https://github.com/purelb/gobgp-netlink.git /tmp/gobgp
          echo "replace github.com/osrg/gobgp/v4 => /tmp/gobgp" >> go.mod

      - name: Setup envtest
        run: |
          go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
          setup-envtest use

      - name: Run integration tests
        run: |
          eval $(setup-envtest use -p env)
          go test -v ./test/integration/...

  build:
    name: Build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Clone gobgp-netlink fork
        run: |
          git clone --depth 1 https://github.com/purelb/gobgp-netlink.git /tmp/gobgp
          echo "replace github.com/osrg/gobgp/v4 => /tmp/gobgp" >> go.mod

      - name: Build manager binary
        run: go build -o bin/manager ./cmd/manager

      - name: Verify CRD generation
        run: |
          make manifests
          git diff --exit-code config/crd/bases/

  docker-build:
    name: Docker Build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Build Docker image (no push)
        uses: docker/build-push-action@v5
        with:
          context: .
          push: false
          tags: ghcr.io/adamdunstan/registry/k8gobgp:ci-${{ github.sha }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
```

### Release Workflow

**New file:** `.github/workflows/release.yaml`
```yaml
name: Release

on:
  push:
    tags:
      - 'v*'

env:
  REGISTRY: ghcr.io
  IMAGE_NAME: adamdunstan/registry/k8gobgp

jobs:
  release:
    name: Build and Release
    runs-on: ubuntu-latest
    permissions:
      contents: write
      packages: write

    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.24'

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to Container Registry
        uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Extract version from tag
        id: version
        run: echo "VERSION=${GITHUB_REF#refs/tags/}" >> $GITHUB_OUTPUT

      - name: Build and push Docker image
        uses: docker/build-push-action@v5
        with:
          context: .
          push: true
          tags: |
            ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:${{ steps.version.outputs.VERSION }}
            ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:latest
          labels: |
            org.opencontainers.image.source=${{ github.server_url }}/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.version=${{ steps.version.outputs.VERSION }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

      - name: Generate CRD manifests
        run: make manifests

      - name: Create install manifest bundle
        run: |
          mkdir -p dist
          cat config/crd/bases/*.yaml > dist/crds.yaml
          cat config/rbac/*.yaml > dist/rbac.yaml
          cat config/daemonset/*.yaml > dist/daemonset.yaml
          # Create all-in-one install manifest
          cat dist/crds.yaml dist/rbac.yaml dist/daemonset.yaml > dist/install.yaml

      - name: Create GitHub Release
        uses: softprops/action-gh-release@v1
        with:
          generate_release_notes: true
          files: |
            dist/install.yaml
            dist/crds.yaml
            dist/rbac.yaml
            dist/daemonset.yaml
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### Dependabot Configuration

**New file:** `.github/dependabot.yaml`
```yaml
version: 2
updates:
  - package-ecosystem: "gomod"
    directory: "/"
    schedule:
      interval: "weekly"
    groups:
      k8s:
        patterns:
          - "k8s.io/*"
          - "sigs.k8s.io/*"

  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"

  - package-ecosystem: "docker"
    directory: "/"
    schedule:
      interval: "weekly"
```

### golangci-lint Configuration

**New file:** `.golangci.yaml`
```yaml
run:
  timeout: 5m
  go: '1.24'

linters:
  enable:
    - errcheck
    - gosimple
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gofmt
    - goimports
    - misspell
    - unconvert
    - unparam
    - gocritic

linters-settings:
  goimports:
    local-prefixes: github.com/adamdunstan/k8gobgp

issues:
  exclude-rules:
    # Exclude generated files
    - path: zz_generated
      linters:
        - all
    # Exclude test files from some linters
    - path: _test\.go
      linters:
        - errcheck
```

### Update Makefile for CI

Add to `Makefile`:
```makefile
.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: verify
verify: fmt vet lint test ## Run all verification checks
	@echo "All checks passed"

.PHONY: ci
ci: verify docker-build ## Full CI pipeline locally
	@echo "CI checks complete"
```

---

## Updated Execution Order (Complete)

| Step | Phase | Description |
|------|-------|-------------|
| 1 | Phase 8 | Rename `api/v1alpha1/` → `api/v1/`, update scheme |
| 2 | Phase 1 | Update go.mod, delete `gobgp_src/` |
| 3 | Phase 2-3 | Update imports and API client |
| 4 | - | Run `go mod tidy` |
| 5 | Phase 7, 7b | Add types (Status, Netlink) |
| 6 | - | Run `make generate && make manifests` |
| 7 | Phase 4-6, 7c | Controller logic changes |
| 8 | Phase 9 | Cleanup main.go |
| 9 | Phase 10 | Update Dockerfile |
| 10 | Phase 12-14 | Config files (samples, entrypoint, daemonset) |
| 11 | Phase 15 | Unit tests |
| 12 | Phase 16 | Integration tests |
| 13 | Phase 17 | Prometheus metrics |
| 14 | Phase 18 | Webhook validation |
| 15 | Phase 19 | GitHub Actions CI/CD |
| 16 | Phase 11 | Final build and test |

---

## Complete Files Summary

| File | Changes |
|------|---------|
| `go.mod` | Update gobgp v3→v4, add test deps (ginkgo, gomega, testify) |
| `go.sum` | Regenerated |
| `api/v1alpha1/` | **RENAME** → `api/v1/` |
| `api/v1/types.go` | Package `v1`, NetlinkConfig, Conditions, printer columns |
| `api/v1/scheme.go` | Package `v1`, group `bgp.purelb.io`, version `v1` |
| `api/v1/bgpconfiguration_webhook.go` | **NEW** - Validation and defaulting webhooks |
| `api/v1/zz_generated.deepcopy.go` | Regenerated |
| `controllers/bgpconfiguration_controller.go` | v4 imports, finalizers, requeue, metrics instrumentation |
| `controllers/bgpconfiguration_controller_test.go` | **NEW** - Unit tests |
| `controllers/grpc_client.go` | **NEW** - Interface for testability |
| `controllers/metrics.go` | **NEW** - Prometheus metrics |
| `test/integration/suite_test.go` | **NEW** - Integration test setup |
| `test/integration/bgpconfiguration_test.go` | **NEW** - Integration tests |
| `cmd/manager/main.go` | v1 import, webhook setup, remove debug |
| `config/crd/bases/` | Regenerated `bgp.purelb.io_bgpconfigurations.yaml` |
| `config/rbac/role.yaml` | Regenerated for `bgp.purelb.io` |
| `config/samples/*.yaml` | Update apiVersion to `bgp.purelb.io/v1` |
| `config/daemonset/gobgp-daemonset.yaml` | Add resources, probes, metrics port |
| `config/prometheus/servicemonitor.yaml` | **NEW** - Prometheus ServiceMonitor |
| `config/webhook/manifests.yaml` | **NEW** - Webhook configurations |
| `config/webhook/service.yaml` | **NEW** - Webhook service |
| `config/webhook/certificate.yaml` | **NEW** - cert-manager certificate |
| `entrypoint.sh` | Add health check wait loop |
| `Dockerfile` | Clone gobgp-netlink, vendor build |
| `Makefile` | Add test, test-integration, lint, verify, ci targets |
| `.github/workflows/ci.yaml` | **NEW** - CI workflow (lint, test, build) |
| `.github/workflows/release.yaml` | **NEW** - Release workflow (build, push, release) |
| `.github/dependabot.yaml` | **NEW** - Dependabot configuration |
| `.golangci.yaml` | **NEW** - golangci-lint configuration |
| `gobgp_src/` | **DELETE** |

---

## Red Team Security & Reliability Review

### Executive Summary

This Red Team review identifies **23 issues** across security, reliability, operational, and architectural domains. **7 are CRITICAL**, **9 are HIGH**, and **7 are MEDIUM** severity.

---

### CRITICAL Issues

#### 1. **SECURITY: Insecure gRPC Communication**
| Severity | CRITICAL |
|----------|----------|
| **Location** | `controllers/bgpconfiguration_controller.go:37`, Plan Phase 6 |
| **Issue** | gRPC uses `insecure.NewCredentials()` - all BGP configuration transmitted in plaintext |
| **Risk** | Man-in-the-middle attack could inject malicious BGP routes, causing traffic hijacking |
| **Attack Vector** | Network-level attacker on same host/network intercepts gRPC traffic |
| **Remediation** | Add mTLS option for gRPC. At minimum, use Unix socket when on same host. Add `--grpc-tls-cert`, `--grpc-tls-key` flags |

#### 2. **SECURITY: BGP Auth Password in Plain Text CRD**
| Severity | CRITICAL |
|----------|----------|
| **Location** | `api/v1alpha1/types.go:71` - `AuthPassword string` |
| **Issue** | BGP MD5 auth passwords stored in plain text in CRD, visible via `kubectl get` |
| **Risk** | Password exposure in etcd, audit logs, kubectl output, backups |
| **Attack Vector** | Any user with read access to BGPConfiguration CRs can retrieve BGP auth secrets |
| **Remediation** | Use `SecretKeyRef` pattern - reference a Kubernetes Secret instead of inline password |

```go
// Proposed fix
type NeighborConfig struct {
    // AuthPasswordSecretRef references a Secret containing the BGP auth password
    AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`
    // REMOVE: AuthPassword string `json:"authPassword,omitempty"`
}
```

#### 3. **SECURITY: Container Runs as Privileged**
| Severity | CRITICAL |
|----------|----------|
| **Location** | `config/daemonset/gobgp-daemonset.yaml:24` |
| **Issue** | `privileged: true` gives container full host access |
| **Risk** | Container escape vulnerability becomes full host compromise |
| **Attack Vector** | Any RCE in gobgpd or controller leads to complete node takeover |
| **Remediation** | Use specific capabilities instead: `NET_ADMIN`, `NET_RAW`, `NET_BIND_SERVICE`. Add `readOnlyRootFilesystem: true` |

```yaml
securityContext:
  privileged: false
  capabilities:
    add:
      - NET_ADMIN
      - NET_RAW
      - NET_BIND_SERVICE
    drop:
      - ALL
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65534
```

#### 4. **SECURITY: No Network Policy / RBAC Least Privilege**
| Severity | CRITICAL |
|----------|----------|
| **Location** | Missing from plan |
| **Issue** | No NetworkPolicy restricting BGP port access; DaemonSet uses `hostNetwork: true` exposing gRPC to all |
| **Risk** | Any pod on the node can connect to gobgpd gRPC on localhost:50051 and reconfigure BGP |
| **Attack Vector** | Compromised workload pod manipulates BGP routing |
| **Remediation** | Add NetworkPolicy, bind gRPC to Unix socket, or use iptables rules in init container |

#### 5. **RELIABILITY: No Graceful Shutdown Handling**
| Severity | CRITICAL |
|----------|----------|
| **Location** | `entrypoint.sh`, Controller |
| **Issue** | No SIGTERM handling - BGP sessions drop abruptly on pod termination |
| **Risk** | Route flapping across cluster during rolling updates, potential traffic blackholes |
| **Attack Vector** | N/A (reliability issue) |
| **Remediation** | Implement graceful shutdown: gobgpd should send BGP NOTIFICATION before exit; add `preStop` hook with delay |

```yaml
lifecycle:
  preStop:
    exec:
      command: ["/bin/sh", "-c", "gobgp neighbor shutdown all && sleep 10"]
terminationGracePeriodSeconds: 60
```

#### 6. **SECURITY: CI Pipeline Clones Untrusted Fork at Runtime**
| Severity | CRITICAL |
|----------|----------|
| **Location** | Plan Phase 19 - CI workflow |
| **Issue** | CI clones `github.com/purelb/gobgp-netlink` at build time with no commit pinning |
| **Risk** | Supply chain attack - malicious code injection if fork is compromised |
| **Attack Vector** | Attacker compromises purelb/gobgp-netlink repo, pushes malicious code, next CI run is infected |
| **Remediation** | Pin to specific commit SHA, use Go module proxy, or vendor dependencies |

```yaml
# Bad
run: git clone --depth 1 https://github.com/purelb/gobgp-netlink.git /tmp/gobgp

# Good - pin to commit
run: |
  git clone https://github.com/purelb/gobgp-netlink.git /tmp/gobgp
  cd /tmp/gobgp && git checkout <KNOWN_GOOD_COMMIT_SHA>
```

#### 7. **ARCHITECTURE: Single Binary = Single Point of Failure**
| Severity | CRITICAL |
|----------|----------|
| **Location** | `entrypoint.sh`, Dockerfile |
| **Issue** | gobgpd and controller run in same container. If controller crashes, gobgpd stops. If gobgpd crashes, no automatic restart |
| **Risk** | BGP session loss on any component failure |
| **Attack Vector** | N/A (architectural issue) |
| **Remediation** | Consider sidecar pattern or separate process supervision (e.g., s6, supervisord). Current `wait` in entrypoint exits on first process death |

---

### HIGH Issues

#### 8. **SECURITY: Webhook Certificates Self-Signed Without Rotation**
| Severity | HIGH |
|----------|------|
| **Location** | Plan Phase 18 - `config/webhook/certificate.yaml` |
| **Issue** | Self-signed certs with no rotation policy |
| **Risk** | Certificate expiry causes webhook failures, blocking all BGPConfiguration changes |
| **Remediation** | Set `renewBefore` in Certificate spec, or use cert-manager's default rotation |

#### 9. **RELIABILITY: Finalizer Cleanup Can Leave Orphaned State**
| Severity | HIGH |
|----------|------|
| **Location** | Plan Phase 4 - `cleanupBGPConfiguration` |
| **Issue** | `cleanupBGPConfiguration` only calls `StopBgp()` and ignores errors. If gRPC is unreachable, finalizer is removed anyway but BGP keeps running |
| **Risk** | Orphaned BGP sessions after CR deletion, ghost routes in network |
| **Remediation** | Retry cleanup with backoff. Only remove finalizer on confirmed cleanup OR after timeout with logged warning |

#### 10. **RELIABILITY: No Rate Limiting on Reconciliation**
| Severity | HIGH |
|----------|------|
| **Location** | Controller |
| **Issue** | Rapid CR updates can overwhelm gobgpd with gRPC calls |
| **Risk** | DoS of BGP daemon, route flapping |
| **Remediation** | Add rate limiter to workqueue, debounce rapid updates |

```go
ctrl.Options{
    RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Minute),
}
```

#### 11. **SECURITY: Base Image `alpine/git` is Unusual**
| Severity | HIGH |
|----------|------|
| **Location** | `Dockerfile:19` |
| **Issue** | Using `alpine/git` as runtime image - includes git binary (attack surface), unclear provenance |
| **Risk** | Unnecessary binaries in production, potential vulnerabilities |
| **Remediation** | Use `alpine:latest` or distroless. Git is not needed at runtime |

#### 12. **RELIABILITY: entrypoint.sh Race Condition**
| Severity | HIGH |
|----------|------|
| **Location** | Plan Phase 13 - improved entrypoint |
| **Issue** | 30-second timeout may not be enough; if gobgpd takes longer, manager starts against unready server |
| **Risk** | Controller crashes on startup, enters crash loop |
| **Remediation** | Add exponential backoff, or have manager handle connection retry internally (which Phase 5 does add) |

#### 13. **SECURITY: No Pod Security Standards**
| Severity | HIGH |
|----------|------|
| **Location** | `config/daemonset/gobgp-daemonset.yaml` |
| **Issue** | No `securityContext` at pod level, no `seccompProfile`, no AppArmor/SELinux annotations |
| **Risk** | Container breakout, privilege escalation |
| **Remediation** | Add pod-level security context with restricted profile |

#### 14. **RELIABILITY: No PodDisruptionBudget**
| Severity | HIGH |
|----------|------|
| **Location** | Missing from plan |
| **Issue** | DaemonSet has no PDB - cluster upgrades can disrupt all BGP speakers simultaneously |
| **Risk** | Complete loss of BGP during maintenance |
| **Remediation** | Add PDB (though DaemonSets have limited PDB support, consider anti-affinity or ordered updates) |

#### 15. **SECURITY: GITHUB_TOKEN Has Write Permissions**
| Severity | HIGH |
|----------|------|
| **Location** | Plan Phase 19 - release.yaml |
| **Issue** | `permissions: contents: write, packages: write` on release workflow |
| **Risk** | If workflow is compromised, attacker can push arbitrary code and packages |
| **Remediation** | Use dedicated PAT with minimal scope, or OIDC for container registry. Consider signed releases |

#### 16. **RELIABILITY: No Leader Election Timeout Tuning**
| Severity | HIGH |
|----------|------|
| **Location** | `cmd/manager/main.go` |
| **Issue** | Default leader election settings may cause slow failover (15s+ lease duration) |
| **Risk** | Extended outage during controller failover |
| **Remediation** | Tune `LeaseDuration`, `RenewDeadline`, `RetryPeriod` for faster failover |

---

### MEDIUM Issues

#### 17. **RELIABILITY: No Backup/Restore for BGP State**
| Severity | MEDIUM |
|----------|--------|
| **Issue** | If CRD is deleted, BGP config is lost. No state backup |
| **Remediation** | Document backup procedures, consider state export to ConfigMap |

#### 18. **OBSERVABILITY: Missing Alerting Rules**
| Severity | MEDIUM |
|----------|--------|
| **Issue** | Prometheus metrics defined but no alerting rules |
| **Remediation** | Add PrometheusRule for: BGP neighbor down, reconcile failures, gRPC errors |

#### 19. **RELIABILITY: Integration Tests Mock gRPC**
| Severity | MEDIUM |
|----------|--------|
| **Location** | Plan Phase 16 |
| **Issue** | Integration tests skip actual gRPC server - won't catch protocol issues |
| **Remediation** | Add e2e tests with real gobgpd in container |

#### 20. **SECURITY: No Image Signing**
| Severity | MEDIUM |
|----------|--------|
| **Issue** | Container images not signed with cosign/sigstore |
| **Remediation** | Add cosign signing to release workflow |

#### 21. **RELIABILITY: CRD Validation Incomplete**
| Severity | MEDIUM |
|----------|--------|
| **Location** | Plan Phase 18 - Webhook |
| **Issue** | Webhook validates ASN/RouterID but not all fields (e.g., invalid community strings, malformed prefixes) |
| **Remediation** | Add comprehensive validation for all BGP-specific fields |

#### 22. **OPERATIONAL: No Upgrade/Migration Path**
| Severity | MEDIUM |
|----------|--------|
| **Issue** | Changing API group from `bgp.example.com` to `bgp.purelb.io` breaks existing CRs |
| **Remediation** | Document migration procedure, consider conversion webhook for gradual migration |

#### 23. **SECURITY: Codecov Token Exposure Risk**
| Severity | MEDIUM |
|----------|--------|
| **Location** | Plan Phase 19 - CI workflow |
| **Issue** | `codecov/codecov-action` without token in public repo could be exploited |
| **Remediation** | Add `CODECOV_TOKEN` secret, or disable if not needed |

---

### Summary of Required Plan Modifications

| Issue # | Required Change |
|---------|-----------------|
| 1 | Add Phase 20: gRPC Security (mTLS or Unix socket option) |
| 2 | Modify Phase 7b: Change `AuthPassword` to `AuthPasswordSecretRef` |
| 3, 13 | Modify Phase 14: Fix DaemonSet security context |
| 4 | Add Phase 21: NetworkPolicy for gRPC protection |
| 5 | Modify Phase 13: Add graceful shutdown, preStop hook |
| 6 | Modify Phase 19: Pin gobgp-netlink to commit SHA |
| 7 | Document as known limitation or redesign entrypoint |
| 8 | Modify Phase 18: Add `renewBefore` to Certificate |
| 9 | Modify Phase 4: Add retry logic to cleanup |
| 10 | Modify Phase 5: Add rate limiter to workqueue |
| 11 | Modify Phase 10: Use `alpine:latest` base image |
| 14 | Add Phase 22: PodDisruptionBudget (or document limitation) |
| 15 | Modify Phase 19: Consider OIDC, add image signing |
| 18 | Add Phase 23: PrometheusRule alerting |
| 20 | Modify Phase 19: Add cosign image signing |
| 22 | Document migration path in README |

---

### Attack Surface Summary

```
┌─────────────────────────────────────────────────────────────────┐
│                        ATTACK SURFACE                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌─────────────┐     gRPC:50051      ┌─────────────────────┐    │
│  │   Any Pod   │ ────(INSECURE)────► │     gobgpd          │    │
│  │  on Node    │     localhost       │  (privileged=true)  │    │
│  └─────────────┘                     └──────────┬──────────┘    │
│                                                  │               │
│  ┌─────────────┐     BGP:179         ┌──────────▼──────────┐    │
│  │  External   │ ◄─────────────────► │   Linux Kernel      │    │
│  │  Router     │   (MD5 optional)    │   Routing Table     │    │
│  └─────────────┘                     └─────────────────────┘    │
│                                                                  │
│  ┌─────────────┐     K8s API         ┌─────────────────────┐    │
│  │   kubectl   │ ────────────────►   │  BGPConfiguration   │    │
│  │   (RBAC)    │                     │  (password visible) │    │
│  └─────────────┘                     └─────────────────────┘    │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

### Recommendation Priority Matrix

| Priority | Issue #s | Effort | Impact |
|----------|----------|--------|--------|
| **P0 - Before Release** | 2, 3, 6 | Low | Critical security fixes |
| **P1 - Before Production** | 1, 4, 5, 11 | Medium | Security + reliability |
| **P2 - Fast Follow** | 8, 9, 10, 15, 20 | Medium | Hardening |
| **P3 - Backlog** | 7, 14, 17, 18, 19, 21, 22, 23 | Varies | Polish |

---

## Phase 20: Security Fixes (P0 - Before Release)

### 20a. Fix BGP Auth Password - Use SecretKeyRef (Issue #2)

**File:** `api/v1/types.go`

Replace inline password with Secret reference:

```go
import corev1 "k8s.io/api/core/v1"

// +kubebuilder:object:generate=true
type NeighborConfig struct {
    // AuthPasswordSecretRef references a Secret containing the BGP MD5 auth password
    // The Secret must be in the same namespace as the BGPConfiguration
    // +optional
    AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`
    // DEPRECATED: AuthPassword is deprecated, use AuthPasswordSecretRef instead
    // This field will be removed in a future version
    // +optional
    AuthPassword    string `json:"authPassword,omitempty"`
    Description       string `json:"description,omitempty"`
    LocalAsn          uint32 `json:"localAsn,omitempty"`
    NeighborAddress   string `json:"neighborAddress"`
    PeerAsn           uint32 `json:"peerAsn"`
    PeerGroup         string `json:"peerGroup,omitempty"`
    AdminDown         bool   `json:"adminDown,omitempty"`
    NeighborInterface string `json:"neighborInterface,omitempty"`
    Vrf               string `json:"vrf,omitempty"`
}

// PeerGroupConfig similarly updated
type PeerGroupConfig struct {
    PeerGroupName string `json:"peerGroupName"`
    Description   string `json:"description,omitempty"`
    PeerAsn       uint32 `json:"peerAsn,omitempty"`
    LocalAsn      uint32 `json:"localAsn,omitempty"`
    // +optional
    AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`
    // DEPRECATED
    AuthPassword  string `json:"authPassword,omitempty"`
}
```

**File:** `controllers/bgpconfiguration_controller.go`

Add helper to resolve password from Secret:

```go
func (r *BGPConfigurationReconciler) resolveAuthPassword(ctx context.Context, namespace string, config *bgpv1.NeighborConfig) (string, error) {
    // Prefer SecretKeyRef over inline password
    if config.AuthPasswordSecretRef != nil {
        secret := &corev1.Secret{}
        err := r.Get(ctx, types.NamespacedName{
            Name:      config.AuthPasswordSecretRef.Name,
            Namespace: namespace,
        }, secret)
        if err != nil {
            return "", fmt.Errorf("failed to get auth password secret: %w", err)
        }
        key := config.AuthPasswordSecretRef.Key
        if key == "" {
            key = "password"
        }
        password, ok := secret.Data[key]
        if !ok {
            return "", fmt.Errorf("key %q not found in secret %s", key, config.AuthPasswordSecretRef.Name)
        }
        return string(password), nil
    }
    // Fallback to deprecated inline password (with warning)
    if config.AuthPassword != "" {
        log.Info("WARNING: Using deprecated inline authPassword, migrate to authPasswordSecretRef")
    }
    return config.AuthPassword, nil
}
```

**Add RBAC for Secrets:**
```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
```

### 20b. Fix Container Security Context (Issue #3, #13)

**File:** `config/daemonset/gobgp-daemonset.yaml`

Replace `privileged: true` with minimal capabilities:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels:
    app: gobgp
  name: gobgp
  namespace: gobgp-system
spec:
  selector:
    matchLabels:
      app: gobgp
  template:
    metadata:
      labels:
        app: gobgp
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      securityContext:
        runAsNonRoot: false  # BGP needs root for port 179
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: gobgp
        image: ghcr.io/adamdunstan/registry/k8gobgp:latest
        imagePullPolicy: Always
        securityContext:
          privileged: false
          capabilities:
            add:
              - NET_ADMIN      # For netlink route manipulation
              - NET_RAW        # For raw socket (BGP MD5)
              - NET_BIND_SERVICE  # For binding to port 179
            drop:
              - ALL
          readOnlyRootFilesystem: true
          allowPrivilegeEscalation: false
        env:
          - name: POD_NAME
            valueFrom:
              fieldRef:
                fieldPath: metadata.name
          - name: NODE_NAME
            valueFrom:
              fieldRef:
                fieldPath: spec.nodeName
        ports:
        - containerPort: 50051
          hostPort: 50051
          name: gobgp-grpc
        - containerPort: 8080
          name: metrics
          protocol: TCP
        - containerPort: 8081
          name: health
          protocol: TCP
        volumeMounts:
        - name: tmp
          mountPath: /tmp
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
          limits:
            memory: "256Mi"
            cpu: "500m"
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "gobgp neighbor shutdown all && sleep 10"]
      volumes:
      - name: tmp
        emptyDir: {}
      terminationGracePeriodSeconds: 60
      tolerations:
      - key: node-role.kubernetes.io/master
        operator: Exists
        effect: NoSchedule
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
      - key: CriticalAddonsOnly
        operator: Exists
      serviceAccountName: gobgp-controller-manager
```

### 20c. Pin gobgp-netlink Commit in CI (Issue #6)

**File:** `.github/workflows/ci.yaml` and `release.yaml`

Update all gobgp-netlink clone steps:

```yaml
env:
  GO_VERSION: '1.24'
  GOBGP_NETLINK_COMMIT: 'HEAD'  # TODO: Pin to specific SHA after first stable release

# In each job that clones:
- name: Clone gobgp-netlink fork (pinned)
  run: |
    git clone https://github.com/purelb/gobgp-netlink.git /tmp/gobgp
    cd /tmp/gobgp
    git checkout ${{ env.GOBGP_NETLINK_COMMIT }}
    echo "Using gobgp-netlink at commit: $(git rev-parse HEAD)"
    cd -
    echo "replace github.com/osrg/gobgp/v4 => /tmp/gobgp" >> go.mod
```

**File:** `Dockerfile`

Pin in Dockerfile as well:

```dockerfile
FROM golang:1.24-alpine AS gobgpd_builder

ARG GOBGP_NETLINK_COMMIT=HEAD

WORKDIR /gobgp_app
RUN apk add --no-cache git
RUN git clone https://github.com/purelb/gobgp-netlink.git . && \
    git checkout ${GOBGP_NETLINK_COMMIT} && \
    echo "Building gobgp-netlink at $(git rev-parse HEAD)"
RUN go build -o gobgpd ./cmd/gobgpd
RUN go build -o gobgp ./cmd/gobgp
```

---

## Phase 21: Security & Reliability Fixes (P1 - Before Production)

### 21a. Use Unix Socket for gRPC (Issue #1, #4)

Since gobgpd and the controller run in the same container, use Unix socket instead of TCP:

**File:** `controllers/bgpconfiguration_controller.go`

```go
const (
    defaultGRPCAddress = "unix:///var/run/gobgp/gobgpd.sock"
    requeueDelay       = 30 * time.Second
)

type BGPConfigurationReconciler struct {
    client.Client
    Log           logr.Logger
    Scheme        *runtime.Scheme
    GRPCAddress   string
    ClientFactory GoBgpClientFactory
}

func (r *BGPConfigurationReconciler) getGRPCAddress() string {
    if r.GRPCAddress != "" {
        return r.GRPCAddress
    }
    return defaultGRPCAddress
}
```

**File:** `entrypoint.sh`

Update to use Unix socket:

```bash
#!/bin/sh
set -e

# Create socket directory
mkdir -p /var/run/gobgp

# Start gobgpd with Unix socket
/usr/local/bin/gobgpd -api-hosts unix:///var/run/gobgp/gobgpd.sock &
GOBGPD_PID=$!

# Trap SIGTERM for graceful shutdown
trap 'echo "Received SIGTERM, shutting down..."; /usr/local/bin/gobgp -u /var/run/gobgp/gobgpd.sock neighbor shutdown all 2>/dev/null; kill $GOBGPD_PID; wait $GOBGPD_PID' TERM

# Wait for gRPC to be ready (up to 60 seconds with backoff)
echo "Waiting for gobgpd gRPC server..."
for i in 1 2 3 4 5 6 7 8 9 10; do
    if /usr/local/bin/gobgp -u /var/run/gobgp/gobgpd.sock global 2>/dev/null; then
        echo "gobgpd is ready"
        break
    fi
    sleep $((i * 2))
done

# Start the controller manager
exec /usr/local/bin/manager --gobgp-address=unix:///var/run/gobgp/gobgpd.sock "$@"
```

**File:** `config/daemonset/gobgp-daemonset.yaml`

Add socket volume:

```yaml
volumeMounts:
- name: tmp
  mountPath: /tmp
- name: gobgp-socket
  mountPath: /var/run/gobgp
volumes:
- name: tmp
  emptyDir: {}
- name: gobgp-socket
  emptyDir: {}
```

### 21b. Graceful Shutdown with preStop Hook (Issue #5)

Already included in 20b DaemonSet update:
- `preStop` hook: `gobgp neighbor shutdown all && sleep 10`
- `terminationGracePeriodSeconds: 60`

### 21c. Fix Base Image (Issue #11)

**File:** `Dockerfile`

```dockerfile
# Use alpine:latest instead of alpine/git
FROM alpine:3.19 AS runtime
WORKDIR /
RUN apk add --no-cache ca-certificates tzdata
COPY --from=gobgpd_builder /gobgp_app/gobgpd /usr/local/bin/gobgpd
COPY --from=gobgpd_builder /gobgp_app/gobgp /usr/local/bin/gobgp
COPY --from=reconciler_builder /k8gobgp_app/manager /usr/local/bin/manager
EXPOSE 50051 8080 8081
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Create non-root user (though we need root for port 179)
RUN adduser -D -u 65534 gobgp
ENTRYPOINT ["/entrypoint.sh"]
```

---

## Phase 22: Hardening Fixes (P2 - Fast Follow)

### 22a. Webhook Certificate Rotation (Issue #8)

**File:** `config/webhook/certificate.yaml`

Add rotation settings:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: gobgp-serving-cert
  namespace: gobgp-system
spec:
  dnsNames:
  - gobgp-webhook-service.gobgp-system.svc
  - gobgp-webhook-service.gobgp-system.svc.cluster.local
  issuerRef:
    kind: Issuer
    name: gobgp-selfsigned-issuer
  secretName: gobgp-webhook-server-cert
  # Rotation settings
  duration: 8760h    # 1 year
  renewBefore: 720h  # 30 days before expiry
  privateKey:
    algorithm: ECDSA
    size: 256
```

### 22b. Finalizer Cleanup with Retry (Issue #9)

**File:** `controllers/bgpconfiguration_controller.go`

```go
const (
    cleanupMaxRetries = 3
    cleanupRetryDelay = 2 * time.Second
)

func (r *BGPConfigurationReconciler) cleanupBGPConfiguration(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, log logr.Logger) error {
    log.Info("Cleaning up BGP configuration")

    var lastErr error
    for i := 0; i < cleanupMaxRetries; i++ {
        _, err := apiClient.StopBgp(ctx, &gobgpapi.StopBgpRequest{})
        if err == nil {
            log.Info("BGP stopped successfully")
            return nil
        }
        lastErr = err
        log.Error(err, "Failed to stop BGP, retrying", "attempt", i+1, "maxRetries", cleanupMaxRetries)
        time.Sleep(cleanupRetryDelay)
    }

    // Log warning but allow deletion to proceed after max retries
    log.Error(lastErr, "WARNING: Failed to stop BGP after max retries, proceeding with finalizer removal. Manual cleanup may be required.")
    return nil  // Return nil to allow finalizer removal
}
```

### 22c. Rate Limiter for Reconciliation (Issue #10)

**File:** `controllers/bgpconfiguration_controller.go`

```go
import (
    "k8s.io/client-go/util/workqueue"
    "time"
)

func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&bgpv1.BGPConfiguration{}).
        WithOptions(controller.Options{
            RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
                1*time.Second,   // Base delay
                5*time.Minute,   // Max delay
            ),
            MaxConcurrentReconciles: 1,  // Serialize BGP config changes
        }).
        WithEventFilter(predicate.GenerationChangedPredicate{}).
        Complete(r)
}
```

### 22d. Image Signing with Cosign (Issue #15, #20)

**File:** `.github/workflows/release.yaml`

Add cosign signing step:

```yaml
jobs:
  release:
    name: Build and Release
    runs-on: ubuntu-latest
    permissions:
      contents: write
      packages: write
      id-token: write  # For OIDC signing

    steps:
      # ... existing steps ...

      - name: Install Cosign
        uses: sigstore/cosign-installer@v3

      - name: Build and push Docker image
        id: build
        uses: docker/build-push-action@v5
        with:
          context: .
          push: true
          tags: |
            ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:${{ steps.version.outputs.VERSION }}
            ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:latest
          labels: |
            org.opencontainers.image.source=${{ github.server_url }}/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.version=${{ steps.version.outputs.VERSION }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

      - name: Sign container image
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: |
          cosign sign --yes ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}@${DIGEST}

      - name: Generate SBOM
        uses: anchore/sbom-action@v0
        with:
          image: ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:${{ steps.version.outputs.VERSION }}
          output-file: dist/sbom.spdx.json

      - name: Attach SBOM to image
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: |
          cosign attach sbom --sbom dist/sbom.spdx.json ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}@${DIGEST}

      # Update release to include SBOM
      - name: Create GitHub Release
        uses: softprops/action-gh-release@v1
        with:
          generate_release_notes: true
          files: |
            dist/install.yaml
            dist/crds.yaml
            dist/rbac.yaml
            dist/daemonset.yaml
            dist/sbom.spdx.json
```

---

## Updated Execution Order (Final)

| Step | Phase | Description |
|------|-------|-------------|
| 1 | Phase 8 | Rename `api/v1alpha1/` → `api/v1/`, update scheme |
| 2 | Phase 1 | Update go.mod, delete `gobgp_src/` |
| 3 | Phase 2-3 | Update imports and API client |
| 4 | - | Run `go mod tidy` |
| 5 | Phase 7, 7b | Add types (Status, Netlink) |
| 6 | **Phase 20a** | Add `AuthPasswordSecretRef`, deprecate inline password |
| 7 | - | Run `make generate && make manifests` |
| 8 | Phase 4-6, 7c | Controller logic (finalizers, requeue, configurable endpoint, netlink) |
| 9 | **Phase 21a** | Add Unix socket support for gRPC |
| 10 | **Phase 22b-c** | Add cleanup retry logic, rate limiter |
| 11 | Phase 9 | Cleanup main.go |
| 12 | Phase 10 + **21c** | Update Dockerfile (alpine base, socket volume) |
| 13 | **Phase 20c** | Pin gobgp-netlink commit |
| 14 | Phase 12-14 + **20b** + **21b** | Config files (samples, entrypoint, daemonset with security) |
| 15 | Phase 15 | Unit tests |
| 16 | Phase 16 | Integration tests |
| 17 | Phase 17 | Prometheus metrics |
| 18 | Phase 18 + **22a** | Webhook validation + cert rotation |
| 19 | Phase 19 + **22d** | GitHub Actions CI/CD + image signing |
| 20 | Phase 11 | Final build and test |

---

## Complete Files Summary (Updated)

| File | Changes |
|------|---------|
| `go.mod` | Update gobgp v3→v4, add test deps, add k8s.io/api for corev1 |
| `go.sum` | Regenerated |
| `api/v1alpha1/` | **RENAME** → `api/v1/` |
| `api/v1/types.go` | Package `v1`, NetlinkConfig, Conditions, **AuthPasswordSecretRef**, printer columns |
| `api/v1/scheme.go` | Package `v1`, group `bgp.purelb.io`, version `v1` |
| `api/v1/bgpconfiguration_webhook.go` | **NEW** - Validation and defaulting webhooks |
| `api/v1/zz_generated.deepcopy.go` | Regenerated |
| `controllers/bgpconfiguration_controller.go` | v4 imports, finalizers, **Unix socket**, **rate limiter**, **cleanup retry**, **secret resolution**, requeue, metrics |
| `controllers/bgpconfiguration_controller_test.go` | **NEW** - Unit tests |
| `controllers/grpc_client.go` | **NEW** - Interface for testability |
| `controllers/metrics.go` | **NEW** - Prometheus metrics |
| `test/integration/suite_test.go` | **NEW** - Integration test setup |
| `test/integration/bgpconfiguration_test.go` | **NEW** - Integration tests |
| `cmd/manager/main.go` | v1 import, webhook setup, remove debug, **Unix socket default** |
| `config/crd/bases/` | Regenerated `bgp.purelb.io_bgpconfigurations.yaml` |
| `config/rbac/role.yaml` | Regenerated for `bgp.purelb.io`, **add secrets read** |
| `config/samples/*.yaml` | Update apiVersion to `bgp.purelb.io/v1` |
| `config/daemonset/gobgp-daemonset.yaml` | **Security context**, resources, probes, **preStop**, **socket volume** |
| `config/prometheus/servicemonitor.yaml` | **NEW** - Prometheus ServiceMonitor |
| `config/webhook/manifests.yaml` | **NEW** - Webhook configurations |
| `config/webhook/service.yaml` | **NEW** - Webhook service |
| `config/webhook/certificate.yaml` | **NEW** - cert-manager certificate **with rotation** |
| `entrypoint.sh` | **Unix socket**, **graceful shutdown**, health check |
| `Dockerfile` | **alpine:3.19 base**, **pinned commit**, socket support |
| `Makefile` | Add test, test-integration, lint, verify, ci targets |
| `.github/workflows/ci.yaml` | **NEW** - CI workflow, **pinned gobgp-netlink** |
| `.github/workflows/release.yaml` | **NEW** - Release workflow, **cosign signing**, **SBOM** |
| `.github/dependabot.yaml` | **NEW** - Dependabot configuration |
| `.golangci.yaml` | **NEW** - golangci-lint configuration |
| `gobgp_src/` | **DELETE** |

---

## Backlog (P3 - Create GitHub Issues Post-Release)

These items should be tracked as GitHub issues after initial release:

| Issue # | Title | Description | Labels |
|---------|-------|-------------|--------|
| #7 | Separate gobgpd and controller processes | Current architecture couples gobgpd and controller in single container. Consider sidecar pattern or process supervisor (s6, supervisord) for independent lifecycle management. | `enhancement`, `reliability` |
| #14 | Add PodDisruptionBudget support | DaemonSets have limited PDB support. Investigate ordered rollout strategies or node-by-node update mechanisms to prevent cluster-wide BGP disruption during upgrades. | `enhancement`, `reliability` |
| #17 | Document backup/restore procedures | Create documentation for backing up and restoring BGPConfiguration CRs. Consider automated state export to ConfigMap. | `documentation` |
| #18 | Add PrometheusRule alerting | Create alerting rules for: BGP neighbor down >5min, reconcile failures >3, gRPC connection errors, high reconcile latency. | `enhancement`, `observability` |
| #19 | Add e2e tests with real gobgpd | Current integration tests mock gRPC server. Add end-to-end tests running real gobgpd in containerized test environment. | `testing` |
| #21 | Comprehensive CRD validation | Extend webhook validation to cover: BGP community string format, prefix validation, timer value ranges, policy syntax validation. | `enhancement`, `validation` |
| #22 | Document CRD migration path | Create migration guide from `bgp.example.com/v1alpha1` to `bgp.purelb.io/v1`. Consider conversion webhook for zero-downtime migration. | `documentation` |
| #23 | Secure Codecov integration | Add `CODECOV_TOKEN` secret or remove codecov integration if not needed. | `security`, `ci` |

---

## Out of Scope (Future Work)

- Helm chart packaging
- Multi-cluster support
- mTLS for gRPC (Unix socket sufficient for same-container communication)
