// Copyright 2025 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

const (
	// Template variable constants
	templateNodeIP         = "${NODE_IP}"
	templateNodeIPv4       = "${NODE_IPV4}"
	templateNodeExternalIP = "${NODE_EXTERNAL_IP}"

	// Default router ID pool for hash-based generation
	defaultRouterIDPool = "10.255.0.0/16"

	// Retry configuration for cloud provider delays
	maxStartupRetryTime    = 5 * time.Minute
	initialRetryDelay      = 1 * time.Second
	maxRetryDelay          = 30 * time.Second
	retryBackoffMultiplier = 2.0

	// Node cache configuration
	nodeCacheTTL = 5 * time.Minute

	// Security limits
	maxRouterIDLength      = 256
	maxAnnotationKeyLength = 253
	minRouterIDPoolSize    = 24 // /24 = 256 addresses minimum
)

// Router ID source constants (for status reporting)
const (
	RouterIDSourceExplicit     = "explicit"
	RouterIDSourceTemplate     = "template"
	RouterIDSourceNodeIPv4     = "node-ipv4"
	RouterIDSourceHashFromNode = "hash-from-node-name"
)

// Event reasons for router ID resolution
const (
	// EventReasonRouterIDResolved is emitted when router ID is successfully resolved
	EventReasonRouterIDResolved = "RouterIDResolved"
	// EventReasonRouterIDFailed is emitted when router ID resolution fails
	EventReasonRouterIDFailed = "RouterIDResolutionFailed"
)

// Allowed template variables (whitelist for security)
var allowedTemplates = map[string]bool{
	templateNodeIP:         true,
	templateNodeIPv4:       true,
	templateNodeExternalIP: true,
}

// annotationPattern matches ${node.annotations['key']} with strict key validation
// Key must follow Kubernetes naming rules: alphanumeric, dots, dashes, slashes
var annotationPattern = regexp.MustCompile(`^\$\{node\.annotations\['([a-zA-Z0-9]([a-zA-Z0-9._/-]*[a-zA-Z0-9])?)']\}$`)

// cachedNode holds a cached node object with expiration
type cachedNode struct {
	node      *corev1.Node
	fetchedAt time.Time
}

// nodeCache provides thread-safe caching of Node objects
type nodeCache struct {
	mu    sync.RWMutex
	cache map[string]*cachedNode
	ttl   time.Duration
}

// newNodeCache creates a new node cache with the specified TTL
func newNodeCache(ttl time.Duration) *nodeCache {
	return &nodeCache{
		cache: make(map[string]*cachedNode),
		ttl:   ttl,
	}
}

// get retrieves a node from cache if still valid, evicting expired entries.
func (c *nodeCache) get(nodeName string) (*corev1.Node, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, exists := c.cache[nodeName]
	if !exists {
		return nil, false
	}

	if time.Since(cached.fetchedAt) > c.ttl {
		delete(c.cache, nodeName)
		return nil, false
	}

	return cached.node, true
}

// set stores a node in the cache
func (c *nodeCache) set(nodeName string, node *corev1.Node) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[nodeName] = &cachedNode{
		node:      node,
		fetchedAt: time.Now(),
	}
}

// RouterIDResolution holds the result of router ID resolution
type RouterIDResolution struct {
	RouterID string
	Source   string
	NodeName string
}

// ResolveRouterID resolves the router ID based on the global spec configuration.
// It handles explicit values, template substitution, and auto-detection with hash fallback.
// This function includes retry logic to handle cloud provider delays in populating node addresses.
func (r *BGPConfigurationReconciler) ResolveRouterID(ctx context.Context, globalSpec *bgpv1.GlobalSpec, log logr.Logger) (*RouterIDResolution, error) {
	// Immutability is handled at the caller level (resolveEffectiveRouterID)
	// via localRouterIDCache — once resolved, a config's router ID won't
	// change during this pod's lifetime.

	routerID := globalSpec.RouterID

	// Input length validation (security: prevent ReDoS)
	if len(routerID) > maxRouterIDLength {
		return nil, fmt.Errorf("routerID exceeds maximum length of %d characters", maxRouterIDLength)
	}

	// Case 1: Explicit IPv4 address
	if routerID != "" && !strings.HasPrefix(routerID, "${") {
		if err := ValidateRouterIDFormat(routerID); err != nil {
			return nil, fmt.Errorf("invalid explicit routerID: %w", err)
		}
		log.V(1).Info("using explicit router ID", "routerID", routerID)
		return &RouterIDResolution{
			RouterID: routerID,
			Source:   RouterIDSourceExplicit,
			NodeName: r.NodeName,
		}, nil
	}

	// For template or auto-detect, we need the node object
	node, err := r.getNodeWithRetry(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %q: %w", r.NodeName, err)
	}

	// Case 2: Template variable
	if strings.HasPrefix(routerID, "${") {
		resolved, resolveErr := r.resolveRouterIDTemplate(routerID, node, log)
		if resolveErr != nil {
			return nil, fmt.Errorf("template resolution failed: %w", resolveErr)
		}
		if validateErr := ValidateRouterIDFormat(resolved); validateErr != nil {
			return nil, fmt.Errorf("resolved template value is invalid: %w", validateErr)
		}
		log.V(1).Info("resolved router ID from template", "template", routerID, "routerID", resolved)
		return &RouterIDResolution{
			RouterID: resolved,
			Source:   RouterIDSourceTemplate,
			NodeName: r.NodeName,
		}, nil
	}

	// Case 3: Auto-detect (routerID is empty)
	pool := globalSpec.RouterIDPool
	if pool == "" {
		pool = defaultRouterIDPool
	}

	resolved, source, err := r.autoDetectRouterID(node, pool, log)
	if err != nil {
		return nil, fmt.Errorf("auto-detect failed: %w", err)
	}

	log.V(1).Info("auto-detected router ID", "routerID", resolved, "source", source)
	return &RouterIDResolution{
		RouterID: resolved,
		Source:   source,
		NodeName: r.NodeName,
	}, nil
}

// getNodeWithRetry fetches the Node object with exponential backoff retry.
// This handles cloud provider delays in populating node.status.addresses.
func (r *BGPConfigurationReconciler) getNodeWithRetry(ctx context.Context, log logr.Logger) (*corev1.Node, error) {
	// Check cache first
	if r.nodeCache != nil {
		if node, ok := r.nodeCache.get(r.NodeName); ok {
			log.V(2).Info("using cached node object", "nodeName", r.NodeName)
			return node, nil
		}
	}

	var node corev1.Node
	delay := initialRetryDelay
	deadline := time.Now().Add(maxStartupRetryTime)

	for {
		err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node)
		if err != nil {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timeout waiting for node %q after %v: %w", r.NodeName, maxStartupRetryTime, err)
			}
			log.V(1).Info("waiting for node object", "nodeName", r.NodeName, "retryIn", delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				delay = time.Duration(float64(delay) * retryBackoffMultiplier)
				if delay > maxRetryDelay {
					delay = maxRetryDelay
				}
				continue
			}
		}

		// Node found, check if it has addresses (cloud provider may still be populating)
		if len(node.Status.Addresses) == 0 {
			if time.Now().After(deadline) {
				// No addresses but we've timed out - continue anyway, hash fallback will be used
				log.Info("node has no addresses after timeout, will use hash-based router ID", "nodeName", r.NodeName)
				break
			}
			log.V(1).Info("waiting for node addresses to be populated", "nodeName", r.NodeName, "retryIn", delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				delay = time.Duration(float64(delay) * retryBackoffMultiplier)
				if delay > maxRetryDelay {
					delay = maxRetryDelay
				}
				continue
			}
		}

		break
	}

	// Cache the node
	if r.nodeCache != nil {
		r.nodeCache.set(r.NodeName, &node)
	}

	return &node, nil
}

// resolveRouterIDTemplate resolves template variables in the router ID string.
// Supports: ${NODE_IP}, ${NODE_IPV4}, ${NODE_EXTERNAL_IP}, ${node.annotations['key']}
func (r *BGPConfigurationReconciler) resolveRouterIDTemplate(template string, node *corev1.Node, log logr.Logger) (string, error) {
	// Security: Check against whitelist first
	if allowedTemplates[template] {
		switch template {
		case templateNodeIP, templateNodeIPv4:
			ip, err := getNodeInternalIPv4(node)
			if err != nil {
				return "", fmt.Errorf("cannot resolve %s: %w", template, err)
			}
			return ip, nil

		case templateNodeExternalIP:
			ip, err := getNodeExternalIPv4(node)
			if err != nil {
				return "", fmt.Errorf("cannot resolve %s: %w", template, err)
			}
			return ip, nil
		}
	}

	// Check for annotation pattern
	matches := annotationPattern.FindStringSubmatch(template)
	if matches == nil {
		return "", fmt.Errorf("unsupported template syntax: %q (allowed: ${NODE_IP}, ${NODE_IPV4}, ${NODE_EXTERNAL_IP}, ${node.annotations['key']})", template)
	}

	annotationKey := matches[1]

	// Security: Validate annotation key length
	if len(annotationKey) > maxAnnotationKeyLength {
		return "", fmt.Errorf("annotation key exceeds maximum length of %d", maxAnnotationKeyLength)
	}

	// Security: Check for injection attempts (null bytes, newlines)
	if strings.ContainsAny(annotationKey, "\x00\n\r") {
		return "", fmt.Errorf("annotation key contains invalid characters")
	}

	value, exists := node.Annotations[annotationKey]
	if !exists {
		return "", fmt.Errorf("annotation %q not found on node %q", annotationKey, node.Name)
	}

	if value == "" {
		return "", fmt.Errorf("annotation %q is empty on node %q", annotationKey, node.Name)
	}

	return value, nil
}

// autoDetectRouterID attempts to detect the router ID from node's IPv4 address,
// falling back to hash-based generation for IPv6-only nodes.
func (r *BGPConfigurationReconciler) autoDetectRouterID(node *corev1.Node, pool string, log logr.Logger) (string, string, error) {
	// Try to get internal IPv4 first
	ipv4, err := getNodeInternalIPv4(node)
	if err == nil {
		return ipv4, RouterIDSourceNodeIPv4, nil
	}

	log.V(1).Info("no internal IPv4 address found, using hash-based generation",
		"nodeName", node.Name, "reason", err.Error())

	// Fall back to hash-based generation
	routerID, err := GenerateRouterIDFromNodeName(node.Name, pool)
	if err != nil {
		return "", "", fmt.Errorf("hash-based generation failed: %w", err)
	}

	return routerID, RouterIDSourceHashFromNode, nil
}

// getNodeInternalIPv4 extracts the first valid internal IPv4 address from a node.
// Returns an error if no valid IPv4 address is found.
func getNodeInternalIPv4(node *corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ip := net.ParseIP(addr.Address)
			if ip == nil {
				continue
			}
			// Check if it's IPv4 (To4() returns nil for IPv6)
			if ip.To4() == nil {
				continue
			}
			// Validate the address
			if err := validateIPv4Address(addr.Address); err != nil {
				continue
			}
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("no valid internal IPv4 address found on node %q", node.Name)
}

// getNodeExternalIPv4 extracts the first valid external IPv4 address from a node.
func getNodeExternalIPv4(node *corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP {
			ip := net.ParseIP(addr.Address)
			if ip == nil {
				continue
			}
			if ip.To4() == nil {
				continue
			}
			if err := validateIPv4Address(addr.Address); err != nil {
				continue
			}
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("no valid external IPv4 address found on node %q", node.Name)
}

// GenerateRouterIDFromNodeName generates a deterministic router ID from a node name
// using FNV-1a hash, mapped to an address within the specified CIDR pool.
func GenerateRouterIDFromNodeName(nodeName string, pool string) (string, error) {
	_, ipNet, err := net.ParseCIDR(pool)
	if err != nil {
		return "", fmt.Errorf("invalid routerIDPool %q: %w", pool, err)
	}

	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return "", fmt.Errorf("routerIDPool must be IPv4 CIDR, got %q", pool)
	}

	// Validate minimum pool size (security: prevent exhaustion)
	if ones > minRouterIDPoolSize {
		return "", fmt.Errorf("routerIDPool must be at least /%d (got /%d) - minimum 256 addresses required", minRouterIDPoolSize, ones)
	}

	poolSize := uint32(1 << (bits - ones))

	// FNV-1a hash of the node name
	h := fnv.New32a()
	_, _ = h.Write([]byte(nodeName))
	hash := h.Sum32()

	// Map hash to offset within pool, avoiding:
	//   .0   (network address)
	//   .255 (broadcast address for /24 subnets)
	// Usable range: [1, poolSize-2] (poolSize-2 usable addresses)
	offset := (hash % (poolSize - 2)) + 1

	// Use BigEndian for cross-platform determinism (amd64/arm64)
	baseIP := binary.BigEndian.Uint32(ipNet.IP.To4())
	resultIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(resultIP, baseIP+offset)

	return resultIP.String(), nil
}

// validateIPv4Address validates that an IP address is a usable IPv4 address.
// Rejects: loopback, link-local, unspecified, and non-IPv4 addresses.
func validateIPv4Address(address string) error {
	ip := net.ParseIP(address)
	if ip == nil {
		return fmt.Errorf("invalid IP address format: %q", address)
	}

	ipv4 := ip.To4()
	if ipv4 == nil {
		return fmt.Errorf("not an IPv4 address: %q", address)
	}

	// Reject loopback (127.0.0.0/8)
	if ipv4[0] == 127 {
		return fmt.Errorf("loopback address not allowed: %q", address)
	}

	// Reject link-local (169.254.0.0/16)
	if ipv4[0] == 169 && ipv4[1] == 254 {
		return fmt.Errorf("link-local address not allowed: %q", address)
	}

	// Reject unspecified (0.0.0.0)
	if ipv4.Equal(net.IPv4zero) {
		return fmt.Errorf("unspecified address not allowed: %q", address)
	}

	return nil
}

// ValidateRouterIDFormat performs comprehensive validation of a router ID value.
// This is the main validation entry point used for all router ID values.
func ValidateRouterIDFormat(routerID string) error {
	if routerID == "" {
		return fmt.Errorf("router ID cannot be empty")
	}

	if len(routerID) > maxRouterIDLength {
		return fmt.Errorf("router ID exceeds maximum length of %d", maxRouterIDLength)
	}

	ip := net.ParseIP(routerID)
	if ip == nil {
		return fmt.Errorf("router ID must be a valid IP address: %q", routerID)
	}

	// BGP router ID must be IPv4 (RFC 4271, RFC 6286)
	if ip.To4() == nil {
		return fmt.Errorf("router ID must be IPv4 (BGP requirement): %q", routerID)
	}

	return validateIPv4Address(routerID)
}

// ValidateRouterIDPool validates a router ID pool CIDR configuration.
func ValidateRouterIDPool(pool string) error {
	if pool == "" {
		return nil // Empty is allowed, will use default
	}

	_, ipNet, err := net.ParseCIDR(pool)
	if err != nil {
		return fmt.Errorf("invalid CIDR format: %w", err)
	}

	// Must be IPv4
	if ipNet.IP.To4() == nil {
		return fmt.Errorf("routerIDPool must be IPv4 CIDR")
	}

	ones, _ := ipNet.Mask.Size()
	if ones > minRouterIDPoolSize {
		return fmt.Errorf("routerIDPool must be at least /%d (got /%d) - minimum 256 addresses required", minRouterIDPoolSize, ones)
	}

	return nil
}

// InitNodeCache initializes the node cache for the reconciler.
// This should be called during controller setup.
func (r *BGPConfigurationReconciler) InitNodeCache() {
	r.nodeCache = newNodeCache(nodeCacheTTL)
}

// emitRouterIDResolvedEvent emits a Normal event for successful router ID resolution
func (r *BGPConfigurationReconciler) emitRouterIDResolvedEvent(obj client.Object, resolution *RouterIDResolution) {
	if r.Recorder == nil {
		return
	}
	message := fmt.Sprintf("Router ID resolved to %s via %s from node %s",
		resolution.RouterID, resolution.Source, resolution.NodeName)
	r.Recorder.Event(obj, corev1.EventTypeNormal, EventReasonRouterIDResolved, message)
}

// emitRouterIDFailedEvent emits a Warning event when router ID resolution fails
func (r *BGPConfigurationReconciler) emitRouterIDFailedEvent(obj client.Object, err error) {
	if r.Recorder == nil {
		return
	}
	// Sanitize the error message to prevent log injection
	message := fmt.Sprintf("Router ID resolution failed: %s", sanitizeLogMessage(err.Error()))
	r.Recorder.Event(obj, corev1.EventTypeWarning, EventReasonRouterIDFailed, message)
}

// annotatePodWithRouterID patches the current pod's annotations with the resolved router ID and ASN.
// Uses a raw JSON merge patch to bypass the controller-runtime cache (which would require
// cluster-wide list/watch on pods). Only the "patch" RBAC verb is needed.
func (r *BGPConfigurationReconciler) annotatePodWithRouterID(ctx context.Context, resolution *RouterIDResolution, asn uint32) error {
	if r.PodName == "" || r.PodNamespace == "" {
		return fmt.Errorf("PodName or PodNamespace not set")
	}

	pod := &corev1.Pod{}
	pod.Name = r.PodName
	pod.Namespace = r.PodNamespace

	patchJSON := fmt.Sprintf(
		`{"metadata":{"annotations":{"bgp.purelb.io/router-id":%q,"bgp.purelb.io/router-id-source":%q,"bgp.purelb.io/asn":"%d"}}}`,
		resolution.RouterID, resolution.Source, asn)

	if err := r.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(patchJSON))); err != nil {
		return fmt.Errorf("failed to patch pod annotations: %w", err)
	}

	return nil
}

// sanitizeLogMessage sanitizes a string for safe logging (prevents log injection)
func sanitizeLogMessage(s string) string {
	// Replace control characters that could be used for log injection
	result := strings.ReplaceAll(s, "\n", " ")
	result = strings.ReplaceAll(result, "\r", " ")
	result = strings.ReplaceAll(result, "\x00", "")
	// Truncate very long messages
	if len(result) > 500 {
		result = result[:500] + "..."
	}
	return result
}
