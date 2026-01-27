# Dynamic Router ID Resolution for k8gobgp

**Status:** Approved Design
**Author:** Engineering Team
**Date:** 2026-01-26
**Reviewed by:** Senior Engineering (10-year Linux/K8s networking experts), Red Team Security

---

## Summary

Add dynamic BGP router ID resolution supporting three configuration methods, with intelligent auto-detection for both IPv4 and IPv6-only nodes.

## Resolution Priority (highest to lowest)

1. **Explicit IPv4** - `routerID: "192.168.1.1"` - used as-is
2. **Template variable** - `routerID: "${NODE_IP}"` or `"${node.annotations['key']}"`
3. **Auto-detect IPv4** - when omitted, use node's primary internal IPv4
4. **Hash-based generation** - for IPv6-only nodes, generate deterministic IPv4 from node name

## Supported Template Variables

| Variable | Description |
|----------|-------------|
| `${NODE_IP}` / `${NODE_IPV4}` | Node's internal IPv4 address |
| `${NODE_EXTERNAL_IP}` | Node's external IPv4 address |
| `${node.annotations['key']}` | Value from node annotation |

## New Global Config Fields

```yaml
spec:
  global:
    asn: 64512
    # routerID omitted - auto-detect or hash-based
    routerIDPool: "10.255.0.0/16"  # Optional: CIDR for hash-based generation
```

- **Default**: `10.255.0.0/16` if not specified
- **Purpose**: Configurable range for hash-based router ID generation (IPv6-only nodes)
- **Validation**: Must be valid IPv4 CIDR with at least /24 (256 addresses minimum)
- **Multi-cluster**: Each cluster MUST use a unique `routerIDPool` to avoid collisions

## New Status Fields

```yaml
status:
  resolvedRouterID: "10.255.0.42"
  routerIDSource: "hash-from-node-name"  # explicit | template | node-ipv4 | hash-from-node-name
  routerIDResolutionTime: "2026-01-26T10:00:00Z"
  routerIDNode: "worker-1"               # Node name used for resolution
  conditions:
  - type: RouterIDResolved
    status: "True"
    reason: AutoDetect
    message: "Router ID resolved from node internal IPv4"
```

## Kubernetes Events

Emit events for observability and debugging:

| Scenario | Type | Reason | Message |
|----------|------|--------|---------|
| Success | Normal | RouterIDResolved | `Router ID resolved to 10.0.0.5 via auto-detect from node worker-1` |
| Template fail | Warning | RouterIDResolutionFailed | `Template resolution failed: annotation 'custom-id' not found` |
| Hash fallback | Normal | RouterIDFallback | `No IPv4 address, using hash-based router ID from pool` |
| Config error | Warning | RouterIDConfigError | `NODE_NAME environment variable not set` |

**Implementation:** Add EventRecorder via `mgr.GetEventRecorderFor("bgpconfiguration-controller")`

---

## Critical Design Decisions (from Senior Review)

### 1. Startup Retry with Backoff
- Cloud providers populate `node.status.addresses` asynchronously (10-60s delay)
- Implement exponential backoff: 1s, 2s, 4s, 8s... up to 5 minutes max
- Fail pod startup if resolution fails after timeout (clear error message)

### 2. NODE_NAME Validation
- Fail fast at startup if `NODE_NAME` env var is not set
- Add validation in `main.go` before manager starts
- Error: `"NODE_NAME environment variable not set - required for dynamic router ID resolution"`

### 3. Address Validation
- Reject loopback addresses (127.x.x.x)
- Reject link-local addresses (169.254.x.x)
- Reject unspecified (0.0.0.0)
- Handle empty `node.status.addresses` array gracefully

### 4. Hash Collision Awareness
- FNV-1a with /16 pool: ~1% collision probability at 300 nodes
- Log resolved router ID at startup for debugging
- Document: recommend larger CIDR for clusters >500 nodes

### 5. Multi-Cluster Safety
- Document requirement for unique `routerIDPool` per cluster
- Router ID collisions across clusters cause routing ambiguity

---

## Security Controls (from Red Team Review)

### 1. Template Injection Prevention (HIGH)
**Threat:** Malicious template syntax could enable path traversal or command injection.

**Mitigations:**
- **Whitelist-based parsing only** - no shell interpolation
- Use strict regex that only matches allowed patterns:
```go
var allowedTemplates = map[string]bool{
    "${NODE_IP}":          true,
    "${NODE_IPV4}":        true,
    "${NODE_EXTERNAL_IP}": true,
}
var annotationPattern = regexp.MustCompile(`^\$\{node\.annotations\['([a-zA-Z0-9]([a-zA-Z0-9._/-]*[a-zA-Z0-9])?)']\}$`)
```
- Validate annotation keys match Kubernetes naming rules
- **Always validate final result is valid IPv4** before use

### 2. Router ID Pool Exhaustion Prevention (HIGH)
**Threat:** Attacker sets `routerIDPool: "192.0.2.1/32"` causing all nodes to get same ID.

**Mitigations:**
- **Minimum pool size: /24** (256 addresses)
- Reject pools smaller than /24 with clear error
- Log warning when pool utilization exceeds 50%

### 3. Router ID Immutability (HIGH)
**Threat:** Changing annotations could trigger session flapping.

**Mitigations:**
- **Lock router ID after first successful resolution**
- Store in status field, never re-resolve once set
- Only allow change via explicit pod restart
- Log warning if configured value differs from locked value

### 4. ReDoS Prevention (MEDIUM)
**Threat:** Crafted input causes exponential regex backtracking.

**Mitigations:**
- Go's `regexp` package uses RE2 (linear time guarantee)
- Limit input length before regex processing (max 256 chars)
- Use simple patterns without nested quantifiers

### 5. Annotation Key Validation (MEDIUM)
**Threat:** Special characters in annotation keys could cause parsing issues.

**Mitigations:**
- Validate keys match: `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*\/)?[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`
- Reject null bytes, newlines, quotes
- Max key length: 253 characters

### 6. RBAC Scope Awareness (MEDIUM)
**Threat:** `nodes:get` permission reveals cluster topology.

**Mitigations:**
- Document that this permission is required and why
- Only request `get` verb (not `list` or `watch`)
- Log which node information is accessed
- Consider downward API for future enhancement

### 7. Log/Metric Injection Prevention (LOW)
**Threat:** Malicious node names could inject false log entries.

**Mitigations:**
- Sanitize all user-controlled strings before logging
- Use existing `sanitizeLabelValue()` for metric labels
- Replace `\n`, `\r`, `\x00` with `_`

---

## Operational Additions (from Gap Analysis)

### Node Object Caching
Reduce Kubernetes API calls by caching Node objects:
```go
type BGPConfigurationReconciler struct {
    // ... existing fields ...
    nodeCache    map[string]*cachedNode
    nodeCacheTTL time.Duration  // 5 minutes
}

type cachedNode struct {
    node      *corev1.Node
    fetchedAt time.Time
}
```

### Migration Guide (README section)
Document upgrade path for existing deployments:
1. **Existing explicit routerID** - No changes required, continues to work
2. **Switching to dynamic** - Remove `routerID` field, pod restart required
3. **Rollback** - Re-add explicit `routerID`, restart pods
4. **Validation checklist** - Verify NODE_NAME env, RBAC permissions, status fields

### Sample Prometheus Alert Rules
Include in `docs/alerting/router-id-alerts.yaml`:
```yaml
groups:
- name: k8gobgp-router-id
  rules:
  - alert: RouterIDResolutionFailed
    expr: increase(k8gobgp_router_id_resolution_total{result="failure"}[5m]) > 0
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "Router ID resolution failures on {{ $labels.pod }}"
```

### Hash Determinism (Multi-Architecture)
Ensure consistent hash across amd64/arm64:
```go
// Use explicit BigEndian for cross-platform determinism
h := fnv.New32a()
h.Write([]byte(nodeName))  // String bytes are endian-neutral
hash := h.Sum32()
// Map to IP using BigEndian explicitly
binary.BigEndian.PutUint32(resultIP, baseIP+offset)
```

---

## Files to Modify

### 1. API Types - `api/v1/types.go`

**GlobalSpec changes:**
```go
type GlobalSpec struct {
    ASN      uint32 `json:"asn"`
    // RouterID - BGP router identifier. When omitted, auto-detected from node IPv4
    // or generated from node name hash. Supports templates: ${NODE_IP}, ${node.annotations['key']}
    // +optional
    RouterID string `json:"routerID,omitempty"`
    // RouterIDPool - CIDR pool for hash-based router ID generation (IPv6-only nodes)
    // Default: "10.255.0.0/16". Each cluster should use a unique pool.
    // +optional
    RouterIDPool string `json:"routerIDPool,omitempty"`
    // ... existing fields
}
```

**Status changes:**
```go
type BGPConfigurationStatus struct {
    // ... existing fields
    ResolvedRouterID       string      `json:"resolvedRouterID,omitempty"`
    RouterIDSource         string      `json:"routerIDSource,omitempty"`
    RouterIDResolutionTime metav1.Time `json:"routerIDResolutionTime,omitempty"`
}
```

### 2. CRD Schema - `config/crd/bases/bgp.purelb.io_configs.yaml`
- Remove `routerID` from `required` list
- Add `routerIDPool` field with description
- Add status fields for resolved router ID

### 3. RBAC - `config/rbac/role.yaml`
```yaml
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get"]
```

### 4. Main - `cmd/manager/main.go`
- Add NODE_NAME validation at startup (fail fast)
- Initialize router ID resolution before manager starts

### 5. Controller - `controllers/bgpconfiguration_controller.go`

**Add EventRecorder to reconciler struct:**
```go
type BGPConfigurationReconciler struct {
    client.Client
    Log           logr.Logger
    Scheme        *runtime.Scheme
    GoBGPEndpoint string
    Recorder      record.EventRecorder  // NEW: for Kubernetes events
    nodeCache     map[string]*cachedNode // NEW: reduce API calls
}
```

**Initialize in main.go:**
```go
Recorder: mgr.GetEventRecorderFor("bgpconfiguration-controller"),
```

**New constants:**
```go
const (
    templateNodeIP         = "${NODE_IP}"
    templateNodeIPv4       = "${NODE_IPV4}"
    templateNodeExternalIP = "${NODE_EXTERNAL_IP}"
    annotationRouterID     = "bgp.purelb.io/router-id"
    envNodeName            = "NODE_NAME"
    defaultRouterIDPool    = "10.255.0.0/16"
    maxStartupRetryTime    = 5 * time.Minute
)
```

**New functions:**
```go
// resolveRouterID - main resolution with retry logic
func (r *BGPConfigurationReconciler) resolveRouterID(ctx, globalSpec, log) (string, string, error)
// Returns: (routerID, source, error)

// resolveRouterIDWithRetry - handles cloud provider delays
func (r *BGPConfigurationReconciler) resolveRouterIDWithRetry(ctx, globalSpec, log) (string, string, error)

// getNodeWithRetry - fetch Node with exponential backoff
func (r *BGPConfigurationReconciler) getNodeWithRetry(ctx, log) (*corev1.Node, error)

// resolveRouterIDTemplate - handle ${...} substitution
func (r *BGPConfigurationReconciler) resolveRouterIDTemplate(template, node, log) (string, error)

// autoDetectRouterID - IPv4 detection with hash fallback
func (r *BGPConfigurationReconciler) autoDetectRouterID(node, routerIDPool, log) (string, string, error)

// generateRouterIDFromNodeName - deterministic hash-based generation
func generateRouterIDFromNodeName(nodeName, pool string) (string, error)

// getNodeInternalIPv4 - extract first valid internal IPv4
func getNodeInternalIPv4(node) (string, error)

// validateRouterIDFormat - comprehensive validation
func validateRouterIDFormat(routerID) error
// Rejects: IPv6, loopback, link-local, unspecified, invalid format
```

**Update reconcileGlobal():**
- Call `resolveRouterID()` before creating GoBGP config
- Update status with resolved router ID and source
- Emit Kubernetes Event on successful resolution

### 6. Validation - `controllers/validation.go`
- Add `ValidateRouterID()` with comprehensive checks
- Add `ValidateRouterIDPool()` for CIDR validation

### 7. Metrics - `controllers/metrics.go` (or inline)

**New metrics:**
```go
k8gobgp_router_id_resolution_total{result="success|failure"}     // Counter
k8gobgp_router_id_resolution_duration_seconds                    // Histogram
k8gobgp_router_id_source{source="explicit|template|node_ipv4|hash"}  // Gauge
```

### 8. Tests - `controllers/bgpconfiguration_controller_test.go`

**Test cases:**

*Functional tests:*
- Explicit IPv4 (backward compatibility)
- Explicit IPv6 (should error)
- `${NODE_IP}` on dual-stack node
- `${NODE_EXTERNAL_IP}` resolution
- `${node.annotations['key']}` resolution
- Auto-detect on IPv4 node
- Auto-detect on IPv6-only node (hash fallback)
- Hash-based is deterministic
- Custom routerIDPool
- Invalid routerIDPool CIDR (error)
- Missing annotation (error)
- Empty node addresses (hash fallback)
- Loopback address rejected
- Link-local address rejected
- NODE_NAME missing (error)
- Retry behavior with delayed addresses

*Security tests:*
- Template injection: `${NODE_IP}; rm -rf /` (reject)
- Path traversal: `${node.annotations['../../etc/passwd']}` (reject)
- Invalid annotation key: `${node.annotations['key\x00hidden']}` (reject)
- Pool too small: `/32` and `/31` (reject, minimum /24)
- Router ID immutability: second resolution returns cached value
- Long input: 10KB annotation value (reject before regex)
- Unicode in annotation key (reject non-ASCII)
- Newline in annotation value (sanitize before logging)

### 9. Documentation - `README.md`

Add section covering:
- Three configuration methods with examples
- Template variable reference
- `routerIDPool` configuration
- Multi-cluster requirements (unique pools)
- IPv6-only node handling
- Troubleshooting: checking resolved router ID in status
- Scaling considerations for large clusters
- Migration guide (explicit -> dynamic)
- Security considerations:
  - RBAC permissions required (`nodes:get`)
  - Why router ID is immutable after first resolution
  - Template syntax restrictions (whitelist only)
  - Pool size requirements (minimum /24)

### 10. E2E Tests - `test/e2e/` (new directory)

**Integration tests against existing test cluster:**
```go
// test/e2e/router_id_resolution_test.go

// Tests run against existing cluster (uses current kubeconfig context)
// Prerequisite: kubectl access to test cluster, k8gobgp deployed

func TestRouterIDResolution_AutoDetect(t *testing.T) {
    // 1. Get node name and IP from test cluster
    // 2. Create BGPConfiguration without routerID
    // 3. Wait for reconciliation
    // 4. Verify status.resolvedRouterID matches node IP
    // 5. Verify Kubernetes events emitted
    // 6. Cleanup: delete BGPConfiguration
}

func TestRouterIDResolution_Template(t *testing.T) {
    // 1. Set annotation on test node
    // 2. Create BGPConfiguration with ${node.annotations['key']}
    // 3. Verify resolution from annotation
    // 4. Cleanup
}

func TestRouterIDResolution_HashFallback(t *testing.T) {
    // 1. Create BGPConfiguration with routerIDPool
    // 2. Simulate IPv6-only by using template that fails
    // 3. Verify hash-based fallback
    // 4. Cleanup
}

func TestRouterIDResolution_Immutability(t *testing.T) {
    // 1. Create BGPConfiguration, wait for resolution
    // 2. Record resolvedRouterID from status
    // 3. Trigger reconcile (update unrelated field)
    // 4. Verify resolvedRouterID unchanged
    // 5. Cleanup
}
```

**Run E2E tests manually:**
```bash
# Ensure kubeconfig points to test cluster
export KUBECONFIG=/path/to/test-cluster/kubeconfig

# Run E2E tests
go test -v -tags=e2e ./test/e2e/...
```

**Makefile target:**
```makefile
.PHONY: test-e2e
test-e2e:
	@echo "Running E2E tests against current kubeconfig context..."
	go test -v -tags=e2e ./test/e2e/...
```

### 11. Alert Rules - `docs/alerting/router-id-alerts.yaml` (new file)

Sample Prometheus alerts for operators to import.

---

## Hash-Based Router ID Generation

```go
func generateRouterIDFromNodeName(nodeName string, pool string) (string, error) {
    _, ipNet, err := net.ParseCIDR(pool)
    if err != nil {
        return "", fmt.Errorf("invalid routerIDPool %q: %w", pool, err)
    }

    ones, bits := ipNet.Mask.Size()
    poolSize := uint32(1 << (bits - ones))

    h := fnv.New32a()
    h.Write([]byte(nodeName))
    hash := h.Sum32()

    // Map hash to offset within pool (avoid .0 network address)
    offset := (hash % (poolSize - 1)) + 1

    baseIP := binary.BigEndian.Uint32(ipNet.IP.To4())
    resultIP := make(net.IP, 4)
    binary.BigEndian.PutUint32(resultIP, baseIP+offset)

    return resultIP.String(), nil
}
```

**Properties:**
- Deterministic: same node name = same router ID
- Configurable pool via `spec.global.routerIDPool`
- Default: `10.255.0.0/16` (65,534 usable addresses)

---

## Implementation Order

1. RBAC update (prerequisite for node access)
2. API types update (RouterID optional, RouterIDPool, status fields, conditions)
3. CRD schema regeneration (`make generate manifests`)
4. Main.go: NODE_NAME validation + EventRecorder initialization
5. Core resolution functions with retry logic and caching
6. Validation functions (template, pool size, address format)
7. Metrics (resolution counter, source gauge)
8. Events (RouterIDResolved, RouterIDFallback, etc.)
9. Integration into reconcileGlobal() with status/condition updates
10. Unit tests (functional + security)
11. E2E tests (test cluster integration)
12. Documentation (README + migration guide)
13. Alert rules (sample Prometheus alerts)

---

## Verification

1. **Build**: `make build`
2. **Unit tests**: `make test`
3. **E2E tests**: `make test-e2e` (requires kubeconfig access to test cluster)
4. **Manual testing:**
   - Explicit routerID (backward compatibility)
   - routerID omitted on IPv4 node (auto-detect)
   - `${NODE_IP}` template
   - `${node.annotations['bgp.purelb.io/router-id']}` annotation
   - IPv6-only node simulation (hash fallback)
   - Custom `routerIDPool: "172.16.0.0/24"`
   - Missing NODE_NAME env var (expect startup failure)
5. **Verify status fields**: `kubectl get bgpconfig -o yaml` shows:
   - `resolvedRouterID`
   - `routerIDSource`
   - `routerIDNode`
   - `conditions` with `RouterIDResolved`
6. **Verify events**: `kubectl describe bgpconfig` shows resolution events
7. **Verify metrics**: `curl localhost:8080/metrics | grep router_id`
8. **Verify logs**: Structured logging with `routerID`, `source`, `nodeName` fields
9. **Security verification:**
   - Test template injection attempts are rejected
   - Verify pool size < /24 is rejected
   - Confirm router ID doesn't change after pod restart (immutability)
   - Check logs don't contain unsanitized user input
10. **Multi-arch verification**: Test on both amd64 and arm64 (hash determinism)
