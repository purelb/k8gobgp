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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testLog returns a no-op logger for tests
var testLog = logr.Discard()

// =============================================================================
// Functional Tests
// =============================================================================

func TestValidateRouterIDFormat(t *testing.T) {
	tests := []struct {
		name      string
		routerID  string
		wantError bool
		errMsg    string
	}{
		// Valid cases
		{
			name:      "valid IPv4",
			routerID:  "192.168.1.1",
			wantError: false,
		},
		{
			name:      "valid private IP",
			routerID:  "10.0.0.1",
			wantError: false,
		},
		{
			name:      "valid public IP",
			routerID:  "8.8.8.8",
			wantError: false,
		},

		// Invalid cases
		{
			name:      "empty string",
			routerID:  "",
			wantError: true,
			errMsg:    "cannot be empty",
		},
		{
			name:      "IPv6 address",
			routerID:  "2001:db8::1",
			wantError: true,
			errMsg:    "must be IPv4",
		},
		{
			name:      "loopback address",
			routerID:  "127.0.0.1",
			wantError: true,
			errMsg:    "loopback",
		},
		{
			name:      "loopback range",
			routerID:  "127.255.255.255",
			wantError: true,
			errMsg:    "loopback",
		},
		{
			name:      "link-local address",
			routerID:  "169.254.1.1",
			wantError: true,
			errMsg:    "link-local",
		},
		{
			name:      "unspecified address",
			routerID:  "0.0.0.0",
			wantError: true,
			errMsg:    "unspecified",
		},
		{
			name:      "invalid format - not IP",
			routerID:  "not-an-ip",
			wantError: true,
			errMsg:    "valid IP",
		},
		{
			name:      "invalid format - missing octets",
			routerID:  "192.168.1",
			wantError: true,
			errMsg:    "valid IP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRouterIDFormat(tt.routerID)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestValidateRouterIDPool(t *testing.T) {
	tests := []struct {
		name      string
		pool      string
		wantError bool
		errMsg    string
	}{
		// Valid cases
		{
			name:      "empty pool (uses default)",
			pool:      "",
			wantError: false,
		},
		{
			name:      "valid /16",
			pool:      "10.255.0.0/16",
			wantError: false,
		},
		{
			name:      "valid /24 (minimum)",
			pool:      "10.255.0.0/24",
			wantError: false,
		},
		{
			name:      "valid /8 (large)",
			pool:      "10.0.0.0/8",
			wantError: false,
		},

		// Invalid cases
		{
			name:      "invalid CIDR format",
			pool:      "not-a-cidr",
			wantError: true,
			errMsg:    "invalid CIDR",
		},
		{
			name:      "IPv6 CIDR",
			pool:      "2001:db8::/32",
			wantError: true,
			errMsg:    "must be IPv4",
		},
		{
			name:      "pool too small /25",
			pool:      "10.0.0.0/25",
			wantError: true,
			errMsg:    "at least /24",
		},
		{
			name:      "pool too small /32",
			pool:      "10.0.0.1/32",
			wantError: true,
			errMsg:    "at least /24",
		},
		{
			name:      "pool too small /31",
			pool:      "10.0.0.0/31",
			wantError: true,
			errMsg:    "at least /24",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRouterIDPool(tt.pool)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestGenerateRouterIDFromNodeName(t *testing.T) {
	tests := []struct {
		name      string
		nodeName  string
		pool      string
		wantError bool
		errMsg    string
	}{
		{
			name:      "valid generation with default pool",
			nodeName:  "worker-1",
			pool:      "10.255.0.0/16",
			wantError: false,
		},
		{
			name:      "valid generation with custom pool",
			nodeName:  "worker-2",
			pool:      "172.16.0.0/24",
			wantError: false,
		},
		{
			name:      "invalid pool CIDR",
			nodeName:  "worker-1",
			pool:      "invalid",
			wantError: true,
			errMsg:    "invalid routerIDPool",
		},
		{
			name:      "pool too small",
			nodeName:  "worker-1",
			pool:      "10.0.0.0/25",
			wantError: true,
			errMsg:    "at least /24",
		},
		{
			name:      "IPv6 pool",
			nodeName:  "worker-1",
			pool:      "2001:db8::/32",
			wantError: true,
			errMsg:    "must be IPv4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GenerateRouterIDFromNodeName(tt.nodeName, tt.pool)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				// Verify result is valid IPv4
				if err := ValidateRouterIDFormat(result); err != nil {
					t.Errorf("generated router ID %q is not valid: %v", result, err)
				}
			}
		})
	}
}

func TestGenerateRouterIDFromNodeName_Deterministic(t *testing.T) {
	// Same node name should always produce the same router ID
	nodeName := "worker-node-123"
	pool := "10.255.0.0/16"

	result1, err := GenerateRouterIDFromNodeName(nodeName, pool)
	if err != nil {
		t.Fatalf("first generation failed: %v", err)
	}

	result2, err := GenerateRouterIDFromNodeName(nodeName, pool)
	if err != nil {
		t.Fatalf("second generation failed: %v", err)
	}

	if result1 != result2 {
		t.Errorf("hash is not deterministic: %q != %q", result1, result2)
	}
}

func TestGenerateRouterIDFromNodeName_DifferentNodes(t *testing.T) {
	// Different node names should (usually) produce different router IDs
	pool := "10.255.0.0/16"

	result1, _ := GenerateRouterIDFromNodeName("worker-1", pool)
	result2, _ := GenerateRouterIDFromNodeName("worker-2", pool)

	if result1 == result2 {
		t.Errorf("different nodes produced same router ID: %q", result1)
	}
}

func TestGenerateRouterIDFromNodeName_AvoidsBroadcastAndNetwork(t *testing.T) {
	// Verify that generated router IDs never end in .0 (network) or .255 (broadcast).
	// Test with a /24 pool where this is most critical.
	pool := "10.255.0.0/24"

	for i := 0; i < 1000; i++ {
		nodeName := fmt.Sprintf("test-node-%d", i)
		routerID, err := GenerateRouterIDFromNodeName(nodeName, pool)
		if err != nil {
			t.Fatalf("unexpected error for node %q: %v", nodeName, err)
		}

		// Parse the last octet
		parts := strings.Split(routerID, ".")
		if len(parts) != 4 {
			t.Fatalf("invalid IP format %q for node %q", routerID, nodeName)
		}
		lastOctet := parts[3]
		if lastOctet == "0" {
			t.Errorf("node %q produced network address %s (last octet .0)", nodeName, routerID)
		}
		if lastOctet == "255" {
			t.Errorf("node %q produced broadcast address %s (last octet .255)", nodeName, routerID)
		}
	}
}

func TestGetNodeInternalIPv4(t *testing.T) {
	tests := []struct {
		name      string
		node      *corev1.Node
		wantIP    string
		wantError bool
	}{
		{
			name: "node with IPv4 internal address",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					},
				},
			},
			wantIP:    "192.168.1.10",
			wantError: false,
		},
		{
			name: "node with multiple addresses - returns first IPv4",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeHostName, Address: "test-node"},
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
					},
				},
			},
			wantIP:    "192.168.1.10",
			wantError: false,
		},
		{
			name: "dual-stack node - returns IPv4",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					},
				},
			},
			wantIP:    "192.168.1.10",
			wantError: false,
		},
		{
			name: "IPv6-only node - returns error",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
						{Type: corev1.NodeInternalIP, Address: "fd00::1"},
					},
				},
			},
			wantError: true,
		},
		{
			name: "node with no addresses",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status:     corev1.NodeStatus{},
			},
			wantError: true,
		},
		{
			name: "node with only external IP",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
					},
				},
			},
			wantError: true,
		},
		{
			name: "node with loopback address - skipped",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "127.0.0.1"},
					},
				},
			},
			wantError: true,
		},
		{
			name: "node with link-local address - skipped",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "169.254.1.1"},
					},
				},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := getNodeInternalIPv4(tt.node)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				if ip != tt.wantIP {
					t.Errorf("expected IP %q, got %q", tt.wantIP, ip)
				}
			}
		})
	}
}

func TestGetNodeExternalIPv4(t *testing.T) {
	tests := []struct {
		name      string
		node      *corev1.Node
		wantIP    string
		wantError bool
	}{
		{
			name: "node with external IPv4",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
					},
				},
			},
			wantIP:    "1.2.3.4",
			wantError: false,
		},
		{
			name: "node with no external IP",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					},
				},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := getNodeExternalIPv4(tt.node)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				if ip != tt.wantIP {
					t.Errorf("expected IP %q, got %q", tt.wantIP, ip)
				}
			}
		})
	}
}

// =============================================================================
// Security Tests
// =============================================================================

func TestResolveRouterIDTemplate_Security(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Annotations: map[string]string{
				"bgp.purelb.io/router-id": "10.0.0.5",
				"valid-key":               "10.0.0.6",
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
			},
		},
	}

	r := &BGPConfigurationReconciler{NodeName: "test-node"}

	tests := []struct {
		name      string
		template  string
		wantError bool
		errMsg    string
	}{
		// Valid templates
		{
			name:      "valid NODE_IP",
			template:  "${NODE_IP}",
			wantError: false,
		},
		{
			name:      "valid NODE_IPV4",
			template:  "${NODE_IPV4}",
			wantError: false,
		},
		{
			name:      "valid NODE_EXTERNAL_IP",
			template:  "${NODE_EXTERNAL_IP}",
			wantError: false,
		},
		{
			name:      "valid annotation",
			template:  "${node.annotations['bgp.purelb.io/router-id']}",
			wantError: false,
		},

		// Security: Template injection attempts
		{
			name:      "injection attempt - semicolon",
			template:  "${NODE_IP}; rm -rf /",
			wantError: true,
			errMsg:    "unsupported template",
		},
		{
			name:      "injection attempt - command substitution",
			template:  "$(whoami)",
			wantError: true,
			errMsg:    "unsupported template",
		},
		{
			name:      "injection attempt - backticks",
			template:  "`id`",
			wantError: true,
			errMsg:    "unsupported template",
		},
		{
			name:      "injection attempt - pipe",
			template:  "${NODE_IP} | cat /etc/passwd",
			wantError: true,
			errMsg:    "unsupported template",
		},

		// Security: Path traversal in annotation key
		// These are rejected by the regex because they don't match valid Kubernetes annotation key patterns
		{
			name:      "path traversal attempt",
			template:  "${node.annotations['../../etc/passwd']}",
			wantError: true,
			errMsg:    "unsupported template", // Rejected by regex - invalid key format
		},
		{
			name:      "path traversal with encoding",
			template:  "${node.annotations['..%2F..%2Fetc%2Fpasswd']}",
			wantError: true,
			errMsg:    "unsupported template", // Rejected by regex - invalid key format
		},

		// Security: Invalid characters in annotation key
		// These are rejected by the regex because they contain invalid characters
		{
			name:      "null byte in annotation key",
			template:  "${node.annotations['key\x00hidden']}",
			wantError: true,
			errMsg:    "unsupported template", // Rejected by regex - null byte doesn't match
		},
		{
			name:      "newline in annotation key",
			template:  "${node.annotations['key\nhidden']}",
			wantError: true,
			errMsg:    "unsupported template", // Won't match the regex
		},

		// Security: Unknown templates
		{
			name:      "unknown template variable",
			template:  "${UNKNOWN_VAR}",
			wantError: true,
			errMsg:    "unsupported template",
		},
		{
			name:      "malformed annotation syntax",
			template:  "${node.annotations[key]}",
			wantError: true,
			errMsg:    "unsupported template",
		},
		{
			name:      "malformed annotation - double quotes",
			template:  "${node.annotations[\"key\"]}",
			wantError: true,
			errMsg:    "unsupported template",
		},

		// Edge cases
		{
			name:      "missing annotation",
			template:  "${node.annotations['nonexistent-key']}",
			wantError: true,
			errMsg:    "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := r.resolveRouterIDTemplate(tt.template, node, testLog)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil (result: %q)", tt.errMsg, result)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestResolveRouterIDTemplate_LongInput(t *testing.T) {
	// Security: Very long annotation key should be rejected
	r := &BGPConfigurationReconciler{NodeName: "test-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}

	// Create a key longer than maxAnnotationKeyLength (253)
	longKey := strings.Repeat("a", 300)
	template := "${node.annotations['" + longKey + "']}"

	_, err := r.resolveRouterIDTemplate(template, node, testLog)
	if err == nil {
		t.Error("expected error for long annotation key, got nil")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Errorf("expected error about maximum length, got %v", err)
	}
}

func TestSanitizeLogMessage(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "normal message",
			input:  "Router ID resolved successfully",
			expect: "Router ID resolved successfully",
		},
		{
			name:   "newline injection",
			input:  "Error: something\nINFO: fake log entry",
			expect: "Error: something INFO: fake log entry",
		},
		{
			name:   "carriage return injection",
			input:  "Error\rINFO: fake",
			expect: "Error INFO: fake",
		},
		{
			name:   "null byte",
			input:  "Error\x00hidden",
			expect: "Errorhidden",
		},
		{
			name:   "very long message truncated",
			input:  strings.Repeat("a", 600),
			expect: strings.Repeat("a", 500) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeLogMessage(tt.input)
			if result != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, result)
			}
		})
	}
}

func TestNodeCache(t *testing.T) {
	cache := newNodeCache(5 * time.Minute)

	// Test set and get
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}

	cache.set("test-node", node)

	retrieved, ok := cache.get("test-node")
	if !ok {
		t.Error("expected to find cached node")
	}
	if retrieved.Name != "test-node" {
		t.Errorf("expected node name 'test-node', got %q", retrieved.Name)
	}

	// Test miss
	_, ok = cache.get("nonexistent")
	if ok {
		t.Error("expected cache miss for nonexistent node")
	}
}

func TestNodeCache_Eviction(t *testing.T) {
	// Use a very short TTL so entries expire immediately
	cache := newNodeCache(1 * time.Nanosecond)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "evict-me"},
	}
	cache.set("evict-me", node)

	// Sleep briefly to ensure TTL has passed
	time.Sleep(1 * time.Millisecond)

	// Should miss due to expiration and evict the entry
	_, ok := cache.get("evict-me")
	if ok {
		t.Error("expected cache miss for expired entry")
	}

	// Verify the entry was actually deleted from the underlying map
	cache.mu.Lock()
	_, exists := cache.cache["evict-me"]
	cache.mu.Unlock()
	if exists {
		t.Error("expected expired entry to be deleted from cache map")
	}
}

// =============================================================================
// Auto-Detect Tests
// =============================================================================

func TestAutoDetectRouterID_IPv4Node(t *testing.T) {
	r := &BGPConfigurationReconciler{NodeName: "test-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			},
		},
	}

	routerID, source, err := r.autoDetectRouterID(node, defaultRouterIDPool, testLog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != RouterIDSourceNodeIPv4 {
		t.Errorf("expected source %q, got %q", RouterIDSourceNodeIPv4, source)
	}
	if routerID != "192.168.1.10" {
		t.Errorf("expected routerID '192.168.1.10', got %q", routerID)
	}
}

func TestAutoDetectRouterID_IPv6OnlyFallback(t *testing.T) {
	r := &BGPConfigurationReconciler{NodeName: "ipv6-only-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ipv6-only-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
			},
		},
	}

	routerID, source, err := r.autoDetectRouterID(node, defaultRouterIDPool, testLog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != RouterIDSourceHashFromNode {
		t.Errorf("expected source %q, got %q", RouterIDSourceHashFromNode, source)
	}
	// Verify the generated router ID is valid IPv4
	if err := ValidateRouterIDFormat(routerID); err != nil {
		t.Errorf("generated router ID %q is invalid: %v", routerID, err)
	}
}

func TestAutoDetectRouterID_NoAddresses(t *testing.T) {
	r := &BGPConfigurationReconciler{NodeName: "empty-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-node"},
		Status:     corev1.NodeStatus{},
	}

	routerID, source, err := r.autoDetectRouterID(node, defaultRouterIDPool, testLog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With no addresses, should fall back to hash-based
	if source != RouterIDSourceHashFromNode {
		t.Errorf("expected source %q, got %q", RouterIDSourceHashFromNode, source)
	}
	if err := ValidateRouterIDFormat(routerID); err != nil {
		t.Errorf("generated router ID %q is invalid: %v", routerID, err)
	}
}

func TestAutoDetectRouterID_CustomPool(t *testing.T) {
	r := &BGPConfigurationReconciler{NodeName: "ipv6-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ipv6-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "fd00::1"},
			},
		},
	}

	routerID, source, err := r.autoDetectRouterID(node, "172.30.0.0/24", testLog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != RouterIDSourceHashFromNode {
		t.Errorf("expected source %q, got %q", RouterIDSourceHashFromNode, source)
	}
	// Verify router ID is in the custom pool range
	if !strings.HasPrefix(routerID, "172.30.0.") {
		t.Errorf("expected router ID in 172.30.0.0/24, got %q", routerID)
	}
}

func TestAutoDetectRouterID_InvalidPool(t *testing.T) {
	r := &BGPConfigurationReconciler{NodeName: "test-node"}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
			},
		},
	}

	_, _, err := r.autoDetectRouterID(node, "invalid-cidr", testLog)
	if err == nil {
		t.Error("expected error for invalid pool CIDR, got nil")
	}
}

// =============================================================================
// Annotation Key Validation Tests
// =============================================================================

func TestAnnotationKeyValidation(t *testing.T) {
	// Test that the annotation pattern regex correctly validates keys
	validKeys := []string{
		"simple",
		"bgp.purelb.io/router-id",
		"kubernetes.io/hostname",
		"node.alpha.kubernetes.io/ttl",
		"my-key",
		"my_key",
		"MY.KEY",
		"a1",
		"1a",
	}

	invalidKeys := []string{
		// These should fail the annotation pattern regex
		"-starts-with-dash",
		".starts-with-dot",
		"ends-with-dash-",
		"ends-with-dot.",
		"", // empty
	}

	for _, key := range validKeys {
		template := "${node.annotations['" + key + "']}"
		if !annotationPattern.MatchString(template) {
			t.Errorf("expected valid key %q to match pattern", key)
		}
	}

	for _, key := range invalidKeys {
		template := "${node.annotations['" + key + "']}"
		if annotationPattern.MatchString(template) {
			t.Errorf("expected invalid key %q to NOT match pattern", key)
		}
	}
}
