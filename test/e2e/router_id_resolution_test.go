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
	defaultTestNamespace = "k8gobgp-system"
	testTimeout          = 2 * time.Minute
	pollInterval         = 5 * time.Second
)

// testNamespace is where k8gobgp is deployed. It is not always
// k8gobgp-system - deployed as a sidecar it lives in its host's namespace - and
// a hardcoded value made this suite unrunnable anywhere else.
var testNamespace = defaultTestNamespace

var (
	k8sClient  client.Client
	kubeClient *kubernetes.Clientset
)

func TestMain(m *testing.M) {
	if ns := os.Getenv("K8GOBGP_NAMESPACE"); ns != "" {
		testNamespace = ns
	}

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

// --- BFD -------------------------------------------------------------------

// TestBFD_Validation exercises the CRD's BFD validation against a real
// apiserver. These are apiserver-side checks, so they run without a BFD-capable
// peer and without BGP establishing at all - which is the point, because the
// rule they cover exists to catch a misconfiguration that has no other symptom.
//
// The rules are also costed statically by the apiserver, so this doubles as the
// check that they fit the CEL budget: a rule over budget makes the CRD
// unloadable, and nothing in `go build` or `go test` sees that.
func TestBFD_Validation(t *testing.T) {
	ctx := context.Background()
	enabled, disabled := true, false

	cases := []struct {
		name       string
		neighbor   bgpv1.Neighbor
		wantReject bool
		because    string
	}{
		{
			name:       "link-local without a zone, BFD enabled",
			wantReject: true,
			because:    "control packets have no interface to leave by; BGP establishes and BFD never leaves Down",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "fe80::1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled},
			},
		},
		{
			name:       "link-local without a zone, uppercase",
			wantReject: true,
			because:    "an IPv6 address is case-insensitive; the rule lowercases before matching",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "FE80::1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled},
			},
		},
		{
			name:    "link-local WITH a zone",
			because: "the zone is what makes it routable, so this is the correct form",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "fe80::1%eth0", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled},
			},
		},
		{
			name:    "link-local without a zone, BFD explicitly disabled",
			because: "this is the whole-block opt-out from a peer group; rejecting it would reject the way you turn BFD off",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "fe80::1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &disabled},
			},
		},
		{
			name:    "link-local without a zone, no BFD block",
			because: "without BFD the zone-less form still works for BGP",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "fe80::1", PeerAsn: 64513},
			},
		},
		{
			name:    "neighborInterface with BFD",
			because: "gobgpd resolves the address and its zone from the interface",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborInterface: "eth1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled},
			},
		},
		{
			name:       "interval below gobgpd's floor",
			wantReject: true,
			because:    "300 is microseconds, not milliseconds - the unit trap the bound exists to catch",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "192.0.2.1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled, RequiredMinimumReceive: 300},
			},
		},
		{
			name:       "detection multiplier below 3",
			wantReject: true,
			because:    "gobgpd rejects it at apply time; better to fail at kubectl apply",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "192.0.2.1", PeerAsn: 64513},
				BFD:    &bgpv1.BFD{Enabled: &enabled, DetectionMultiplier: 1},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := &bgpv1.BGPConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-bfd-" + randomSuffix(),
					Namespace: testNamespace,
				},
				Spec: bgpv1.BGPConfigurationSpec{
					Global:    bgpv1.GlobalSpec{ASN: 64512},
					Neighbors: []bgpv1.Neighbor{tc.neighbor},
				},
			}

			// DryRunAll: this is a schema assertion, and creating the object for
			// real would make a second BGPConfiguration in the cluster, which
			// this controller deliberately ignores.
			err := k8sClient.Create(ctx, config, client.DryRunAll)

			switch {
			case tc.wantReject && err == nil:
				t.Errorf("expected rejection (%s), but the apiserver accepted it", tc.because)
			case tc.wantReject && err != nil:
				t.Logf("rejected as expected: %v", err)
			case !tc.wantReject && err != nil:
				t.Errorf("expected acceptance (%s), got: %v", tc.because, err)
			}
		})
	}
}

// TestBFD_DefaultsAppliedByAPIServer checks that a bfd block is defaulted on
// write, which is what makes whole-block inheritance work: the block a neighbor
// overrides its peer group with is always complete, never a partial merge.
func TestBFD_DefaultsAppliedByAPIServer(t *testing.T) {
	ctx := context.Background()
	enabled := true

	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bfd-defaults-" + randomSuffix(),
			Namespace: testNamespace,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{ASN: 64512},
			Neighbors: []bgpv1.Neighbor{
				// Only enabled is set; everything else must come back defaulted.
				{
					Config: bgpv1.NeighborConfig{NeighborAddress: "192.0.2.1", PeerAsn: 64513},
					BFD:    &bgpv1.BFD{Enabled: &enabled},
				},
				// No bfd block at all must stay absent, not become a defaulted
				// one - absent is how a neighbor says "inherit".
				{
					Config: bgpv1.NeighborConfig{NeighborAddress: "192.0.2.2", PeerAsn: 64513},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, config, client.DryRunAll); err != nil {
		t.Fatalf("Failed to create BGPConfiguration: %v", err)
	}

	got := config.Spec.Neighbors[0].BFD
	if got == nil {
		t.Fatal("bfd block disappeared")
	}
	// These must match bfdWithDefaults in the controller, or a peer that sets a
	// bfd block compares unequal against what ListPeer echoes and churns.
	for _, c := range []struct {
		field string
		got   uint32
		want  uint32
	}{
		{"port", got.Port, 3784},
		{"desiredMinimumTxInterval", got.DesiredMinimumTxInterval, 1000000},
		{"requiredMinimumReceive", got.RequiredMinimumReceive, 1000000},
		{"detectionMultiplier", got.DetectionMultiplier, 3},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.field, c.got, c.want)
		}
	}
	t.Logf("apiserver-defaulted bfd: %+v", *got)

	if config.Spec.Neighbors[1].BFD != nil {
		t.Errorf("a neighbor with no bfd block gained one (%+v); absent must stay absent so it inherits",
			*config.Spec.Neighbors[1].BFD)
	}
}

// TestBFD_NonBFDNodesStayHealthy is the regression guard for the health gate.
//
// The BFD socket binds lazily on the first BFD peer, so a cluster not using BFD
// reports bfdServer.listening false on every node. deriveHealth is a pure AND;
// consuming that unguarded marks every such node - including every
// local-address-mode deployment, which peers with nobody - permanently
// unhealthy. This asserts the reported state of the cluster as it actually runs.
func TestBFD_NonBFDNodesStayHealthy(t *testing.T) {
	ctx := context.Background()

	var statuses bgpv1.BGPNodeStatusList
	if err := k8sClient.List(ctx, &statuses); err != nil {
		t.Fatalf("Failed to list BGPNodeStatus: %v", err)
	}
	if len(statuses.Items) == 0 {
		t.Skip("no BGPNodeStatus objects; k8gobgp is not reporting on this cluster")
	}

	for _, s := range statuses.Items {
		st := s.Status
		usesBFD := false
		for _, n := range st.Neighbors {
			if n.BFD != nil {
				usesBFD = true
			}
		}

		var bfdCond *metav1.Condition
		for i := range st.Conditions {
			if st.Conditions[i].Type == "BFDDegraded" {
				bfdCond = &st.Conditions[i]
			}
		}

		listening := st.BFDServer != nil && st.BFDServer.Listening
		t.Logf("%s: usesBFD=%t listening=%t healthy=%t bfdCondition=%v",
			st.NodeName, usesBFD, listening, st.Healthy, bfdCond != nil)

		if usesBFD {
			continue // the degraded path is asserted by the controller unit tests
		}
		if bfdCond != nil {
			t.Errorf("%s has no BFD neighbors but reports a BFDDegraded condition (%s); "+
				"a non-BFD node must carry no BFD condition at all",
				st.NodeName, bfdCond.Reason)
		}
		if !st.Healthy && allNeighborsEstablished(st) {
			t.Errorf("%s has every neighbor Established and no BFD, but reports healthy=false "+
				"(bfdServer.listening=%t) - the health gate has regressed", st.NodeName, listening)
		}
	}
}

func allNeighborsEstablished(st bgpv1.BGPNodeStatusData) bool {
	for _, n := range st.Neighbors {
		if n.State != "Established" {
			return false
		}
	}
	return true
}
