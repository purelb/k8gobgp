//go:build e2e
// +build e2e

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

// Package e2e contains end-to-end tests for k8gobgp.
// These tests require a running Kubernetes cluster accessible via kubeconfig.
//
// Run with: go test -v -tags=e2e ./test/e2e/...
// Or use: make test-e2e
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

const (
	testNamespace = "k8gobgp-system"
	testTimeout   = 2 * time.Minute
	pollInterval  = 5 * time.Second
)

var (
	k8sClient  client.Client
	kubeClient *kubernetes.Clientset
)

func TestMain(m *testing.M) {
	// Setup: Create Kubernetes clients
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("Failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Create standard kubernetes client
	kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("Failed to create kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Create controller-runtime client with our scheme
	scheme, err := bgpv1.SchemeBuilder.Build()
	if err != nil {
		fmt.Printf("Failed to build scheme: %v\n", err)
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		fmt.Printf("Failed to add core scheme: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Printf("Failed to create controller-runtime client: %v\n", err)
		os.Exit(1)
	}

	// Run tests
	os.Exit(m.Run())
}

// TestRouterIDResolution_AutoDetect tests that router ID is auto-detected from node IP
func TestRouterIDResolution_AutoDetect(t *testing.T) {
	ctx := context.Background()

	// Get a node from the cluster to determine expected source
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Skip("No nodes available in cluster")
	}

	node := &nodes.Items[0]
	expectedIP := getNodeInternalIPv4(node)
	if expectedIP == "" {
		t.Logf("Node %s has no IPv4 address, expecting hash-based router ID", node.Name)
	}

	// Create BGPConfiguration without explicit routerID
	configName := "test-auto-detect-" + randomSuffix()
	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN: 64512,
				// RouterID intentionally omitted - should auto-detect
			},
		},
	}

	// Create the config
	if err := k8sClient.Create(ctx, config); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}
	defer cleanup(t, ctx, config)

	// Wait for routerIDSource to be set in status (shared status shows resolution method)
	err = wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		return config.Status.RouterIDSource != "", nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for router ID resolution: %v", err)
	}

	t.Logf("Router ID source: %s", config.Status.RouterIDSource)

	if expectedIP != "" {
		if config.Status.RouterIDSource != "node-ipv4" && config.Status.RouterIDSource != "hash-from-node-name" {
			t.Errorf("Expected source 'node-ipv4' or 'hash-from-node-name', got %s", config.Status.RouterIDSource)
		}
	} else {
		if config.Status.RouterIDSource != "hash-from-node-name" {
			t.Errorf("Expected source 'hash-from-node-name' for IPv6-only node, got %s", config.Status.RouterIDSource)
		}
	}

	// Verify the RouterIDResolved condition is True
	if !hasCondition(config, "RouterIDResolved", "True") {
		t.Errorf("Expected RouterIDResolved condition to be True, conditions: %+v", config.Status.Conditions)
	}
}

// TestRouterIDResolution_Explicit tests that explicit router ID is used as-is
func TestRouterIDResolution_Explicit(t *testing.T) {
	ctx := context.Background()

	configName := "test-explicit-" + randomSuffix()
	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN:      64512,
				RouterID: "10.99.99.99",
			},
		},
	}

	if err := k8sClient.Create(ctx, config); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}
	defer cleanup(t, ctx, config)

	// Wait for routerIDSource to be set
	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		return config.Status.RouterIDSource != "", nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for router ID resolution: %v", err)
	}

	t.Logf("Router ID source: %s", config.Status.RouterIDSource)

	if config.Status.RouterIDSource != "explicit" {
		t.Errorf("Expected source 'explicit', got %s", config.Status.RouterIDSource)
	}
	if !hasCondition(config, "RouterIDResolved", "True") {
		t.Errorf("Expected RouterIDResolved condition to be True, conditions: %+v", config.Status.Conditions)
	}
}

// TestRouterIDResolution_Template tests template-based router ID resolution
func TestRouterIDResolution_Template(t *testing.T) {
	ctx := context.Background()

	configName := "test-template-" + randomSuffix()
	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN:      64512,
				RouterID: "${NODE_IP}",
			},
		},
	}

	if err := k8sClient.Create(ctx, config); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}
	defer cleanup(t, ctx, config)

	// Wait for routerIDSource to be set
	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		return config.Status.RouterIDSource != "", nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for router ID resolution: %v", err)
	}

	t.Logf("Router ID source: %s", config.Status.RouterIDSource)

	if config.Status.RouterIDSource != "template" {
		t.Errorf("Expected source 'template', got %s", config.Status.RouterIDSource)
	}
	if !hasCondition(config, "RouterIDResolved", "True") {
		t.Errorf("Expected RouterIDResolved condition to be True, conditions: %+v", config.Status.Conditions)
	}
}

// TestRouterIDResolution_Immutability tests that the router ID source stays consistent
// after re-reconciliation. The shared status only contains the resolution method
// (not per-node IPs), so it should remain stable across reconcile cycles.
func TestRouterIDResolution_Immutability(t *testing.T) {
	ctx := context.Background()

	configName := "test-immutable-" + randomSuffix()
	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN: 64512,
				// Auto-detect
			},
		},
	}

	if err := k8sClient.Create(ctx, config); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}
	defer cleanup(t, ctx, config)

	// Wait for initial resolution
	var initialSource string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		if config.Status.RouterIDSource != "" {
			initialSource = config.Status.RouterIDSource
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for initial router ID resolution: %v", err)
	}

	t.Logf("Initial source: %s", initialSource)

	// Trigger a reconciliation by updating an unrelated field
	config.Spec.Global.ListenPort = 1179
	if err := k8sClient.Update(ctx, config); err != nil {
		t.Fatalf("Failed to update BGPConfiguration: %v", err)
	}

	// Wait for reconciliation to complete by polling until the observed generation catches up
	err = wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		// Check that the controller has processed the updated generation
		for _, c := range config.Status.Conditions {
			if c.Type == "RouterIDResolved" && c.ObservedGeneration >= config.Generation {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for reconciliation after update: %v", err)
	}

	t.Logf("After update source: %s", config.Status.RouterIDSource)

	// The resolution method should remain stable
	if config.Status.RouterIDSource != initialSource {
		t.Errorf("Router ID source changed! Initial: %s, Current: %s",
			initialSource, config.Status.RouterIDSource)
	}

	// RouterIDResolved condition should still be True
	if !hasCondition(config, "RouterIDResolved", "True") {
		t.Errorf("Expected RouterIDResolved condition to be True after update")
	}
}

// TestRouterIDResolution_CustomPool tests custom router ID pool for hash-based generation
func TestRouterIDResolution_CustomPool(t *testing.T) {
	ctx := context.Background()

	configName := "test-custom-pool-" + randomSuffix()
	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN:          64512,
				RouterIDPool: "172.30.0.0/24",
				// RouterID omitted - will use auto-detect or hash
			},
		},
	}

	if err := k8sClient.Create(ctx, config); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}
	defer cleanup(t, ctx, config)

	// Wait for routerIDSource to be set
	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName, Namespace: testNamespace}, config); err != nil {
			return false, nil
		}
		return config.Status.RouterIDSource != "", nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for router ID resolution: %v", err)
	}

	t.Logf("Router ID source: %s", config.Status.RouterIDSource)

	// Verify resolution succeeded (source is set and condition is True).
	// Per-node router ID details (including pool membership) are available
	// via Prometheus metrics: k8gobgp_router_id_info{router_id, source, node, asn}
	if config.Status.RouterIDSource != "node-ipv4" && config.Status.RouterIDSource != "hash-from-node-name" {
		t.Errorf("Expected source 'node-ipv4' or 'hash-from-node-name', got %s", config.Status.RouterIDSource)
	}
	if !hasCondition(config, "RouterIDResolved", "True") {
		t.Errorf("Expected RouterIDResolved condition to be True, conditions: %+v", config.Status.Conditions)
	}
}

// Helper functions

func cleanup(t *testing.T, ctx context.Context, config *bgpv1.BGPConfiguration) {
	if err := k8sClient.Delete(ctx, config); err != nil {
		t.Logf("Warning: Failed to cleanup BGPConfiguration %s: %v", config.Name, err)
	}
}

func getNodeInternalIPv4(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			// Check if it's IPv4 (simple heuristic: contains dot, no colon)
			if strings.Contains(addr.Address, ".") && !strings.Contains(addr.Address, ":") {
				return addr.Address
			}
		}
	}
	return ""
}

// hasCondition checks whether the BGPConfiguration has a condition with the given type and status
func hasCondition(config *bgpv1.BGPConfiguration, condType, status string) bool {
	for _, c := range config.Status.Conditions {
		if c.Type == condType && string(c.Status) == status {
			return true
		}
	}
	return false
}

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1000000000)
}
